package sftp

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"restic"
	"strings"
	"time"

	"restic/errors"

	"restic/backend"
	"restic/debug"

	"github.com/pkg/sftp"
)

const (
	tempfileRandomSuffixLength = 10
)

// SFTP is a backend in a directory accessed via SFTP.
type SFTP struct {
	c *sftp.Client
	p string

	cmd    *exec.Cmd
	result <-chan error

	backend.Layout
	Config
}

var _ restic.Backend = &SFTP{}

const defaultLayout = "default"

func startClient(program string, args ...string) (*SFTP, error) {
	debug.Log("start client %v %v", program, args)
	// Connect to a remote host and request the sftp subsystem via the 'ssh'
	// command.  This assumes that passwordless login is correctly configured.
	cmd := exec.Command(program, args...)

	// prefix the errors with the program name
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StderrPipe")
	}

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			fmt.Fprintf(os.Stderr, "subprocess %v: %v\n", program, sc.Text())
		}
	}()

	// ignore signals sent to the parent (e.g. SIGINT)
	cmd.SysProcAttr = ignoreSigIntProcAttr()

	// get stdin and stdout
	wr, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdinPipe")
	}
	rd, err := cmd.StdoutPipe()
	if err != nil {
		return nil, errors.Wrap(err, "cmd.StdoutPipe")
	}

	// start the process
	if err := cmd.Start(); err != nil {
		return nil, errors.Wrap(err, "cmd.Start")
	}

	// wait in a different goroutine
	ch := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		debug.Log("ssh command exited, err %v", err)
		ch <- errors.Wrap(err, "cmd.Wait")
	}()

	// open the SFTP session
	client, err := sftp.NewClientPipe(rd, wr)
	if err != nil {
		return nil, errors.Errorf("unable to start the sftp session, error: %v", err)
	}

	return &SFTP{c: client, cmd: cmd, result: ch}, nil
}

func paths(dir string) []string {
	return []string{
		dir,
		Join(dir, backend.Paths.Data),
		Join(dir, backend.Paths.Snapshots),
		Join(dir, backend.Paths.Index),
		Join(dir, backend.Paths.Locks),
		Join(dir, backend.Paths.Keys),
		Join(dir, backend.Paths.Temp),
	}
}

// clientError returns an error if the client has exited. Otherwise, nil is
// returned immediately.
func (r *SFTP) clientError() error {
	select {
	case err := <-r.result:
		debug.Log("client has exited with err %v", err)
		return err
	default:
	}

	return nil
}

// Open opens an sftp backend as described by the config by running
// "ssh" with the appropriate arguments (or cfg.Command, if set).
func Open(cfg Config) (*SFTP, error) {
	debug.Log("open backend with config %#v", cfg)

	cmd, args, err := buildSSHCommand(cfg)
	if err != nil {
		return nil, err
	}

	sftp, err := startClient(cmd, args...)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	sftp.Layout, err = backend.ParseLayout(sftp, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	// test if all necessary dirs and files are there
	for _, d := range paths(cfg.Path) {
		if _, err := sftp.c.Lstat(d); err != nil {
			return nil, errors.Errorf("%s does not exist", d)
		}
	}

	debug.Log("layout: %v\n", sftp.Layout)

	sftp.Config = cfg
	sftp.p = cfg.Path
	return sftp, nil
}

// Join combines path components with slashes (according to the sftp spec).
func (r *SFTP) Join(p ...string) string {
	return path.Join(p...)
}

// ReadDir returns the entries for a directory.
func (r *SFTP) ReadDir(dir string) ([]os.FileInfo, error) {
	return r.c.ReadDir(dir)
}

// IsNotExist returns true if the error is caused by a not existing file.
func (r *SFTP) IsNotExist(err error) bool {
	statusError, ok := err.(*sftp.StatusError)
	if !ok {
		return false
	}

	return statusError.Error() == `sftp: "No such file" (SSH_FX_NO_SUCH_FILE)`
}

func buildSSHCommand(cfg Config) (cmd string, args []string, err error) {
	if cfg.Command != "" {
		return SplitShellArgs(cfg.Command)
	}

	cmd = "ssh"

	hostport := strings.Split(cfg.Host, ":")
	args = []string{hostport[0]}
	if len(hostport) > 1 {
		args = append(args, "-p", hostport[1])
	}
	if cfg.User != "" {
		args = append(args, "-l")
		args = append(args, cfg.User)
	}
	args = append(args, "-s")
	args = append(args, "sftp")
	return cmd, args, nil
}

// Create creates an sftp backend as described by the config by running
// "ssh" with the appropriate arguments (or cfg.Command, if set).
func Create(cfg Config) (*SFTP, error) {
	cmd, args, err := buildSSHCommand(cfg)
	if err != nil {
		return nil, err
	}

	sftp, err := startClient(cmd, args...)
	if err != nil {
		debug.Log("unable to start program: %v", err)
		return nil, err
	}

	sftp.Layout, err = backend.ParseLayout(sftp, cfg.Layout, defaultLayout, cfg.Path)
	if err != nil {
		return nil, err
	}

	// test if config file already exists
	_, err = sftp.c.Lstat(Join(cfg.Path, backend.Paths.Config))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data, refs and temp blobs
	for _, d := range paths(cfg.Path) {
		err = sftp.mkdirAll(d, backend.Modes.Dir)
		debug.Log("mkdirAll %v -> %v", d, err)
		if err != nil {
			return nil, err
		}
	}

	err = sftp.Close()
	if err != nil {
		return nil, errors.Wrap(err, "Close")
	}

	// open backend
	return Open(cfg)
}

// Location returns this backend's location (the directory name).
func (r *SFTP) Location() string {
	return r.p
}

func (r *SFTP) mkdirAll(dir string, mode os.FileMode) error {
	// check if directory already exists
	fi, err := r.c.Lstat(dir)
	if err == nil {
		if fi.IsDir() {
			return nil
		}

		return errors.Errorf("mkdirAll(%s): entry exists but is not a directory", dir)
	}

	// create parent directories
	errMkdirAll := r.mkdirAll(path.Dir(dir), backend.Modes.Dir)

	// create directory
	errMkdir := r.c.Mkdir(dir)

	// test if directory was created successfully
	fi, err = r.c.Lstat(dir)
	if err != nil {
		// return previous errors
		return errors.Errorf("mkdirAll(%s): unable to create directories: %v, %v", dir, errMkdirAll, errMkdir)
	}

	if !fi.IsDir() {
		return errors.Errorf("mkdirAll(%s): entry exists but is not a directory", dir)
	}

	// set mode
	return r.c.Chmod(dir, mode)
}

// Rename temp file to final name according to type and name.
func (r *SFTP) renameFile(oldname string, h restic.Handle) error {
	filename := r.filename(h)

	// create directories if necessary
	if h.Type == restic.DataFile {
		err := r.mkdirAll(path.Dir(filename), backend.Modes.Dir)
		if err != nil {
			return err
		}
	}

	// test if new file exists
	if _, err := r.c.Lstat(filename); err == nil {
		return errors.Errorf("Close(): file %v already exists", filename)
	}

	err := r.c.Rename(oldname, filename)
	if err != nil {
		return errors.Wrap(err, "Rename")
	}

	// set mode to read-only
	fi, err := r.c.Lstat(filename)
	if err != nil {
		return errors.Wrap(err, "Lstat")
	}

	err = r.c.Chmod(filename, fi.Mode()&os.FileMode(^uint32(0222)))
	return errors.Wrap(err, "Chmod")
}

// Join joins the given paths and cleans them afterwards. This always uses
// forward slashes, which is required by sftp.
func Join(parts ...string) string {
	return path.Clean(path.Join(parts...))
}

// Construct path for given restic.Type and name.
func (r *SFTP) filename(h restic.Handle) string {
	if h.Type == restic.ConfigFile {
		return Join(r.p, "config")
	}

	return Join(r.dirname(h), h.Name)
}

// Construct directory for given backend.Type.
func (r *SFTP) dirname(h restic.Handle) string {
	var n string
	switch h.Type {
	case restic.DataFile:
		n = backend.Paths.Data
		if len(h.Name) > 2 {
			n = Join(n, h.Name[:2])
		}
	case restic.SnapshotFile:
		n = backend.Paths.Snapshots
	case restic.IndexFile:
		n = backend.Paths.Index
	case restic.LockFile:
		n = backend.Paths.Locks
	case restic.KeyFile:
		n = backend.Paths.Keys
	}
	return Join(r.p, n)
}

// Save stores data in the backend at the handle.
func (r *SFTP) Save(h restic.Handle, rd io.Reader) (err error) {
	debug.Log("Save %v", h)
	if err := r.clientError(); err != nil {
		return err
	}

	if err := h.Valid(); err != nil {
		return err
	}

	filename := r.Filename(h)

	// create directories if necessary
	if h.Type == restic.DataFile {
		err := r.mkdirAll(path.Dir(filename), backend.Modes.Dir)
		if err != nil {
			return err
		}
	}

	// create new file
	f, err := r.c.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY)
	if err != nil {
		return errors.Wrap(err, "OpenFile")
	}

	// save data
	_, err = io.Copy(f, rd)
	if err != nil {
		f.Close()
		return errors.Wrap(err, "Write")
	}

	err = f.Close()
	if err != nil {
		return errors.Wrap(err, "Close")
	}

	// set mode to read-only
	fi, err := r.c.Lstat(filename)
	if err != nil {
		return errors.Wrap(err, "Lstat")
	}

	err = r.c.Chmod(filename, fi.Mode()&os.FileMode(^uint32(0222)))
	return errors.Wrap(err, "Chmod")
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (r *SFTP) Load(h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v", h, length, offset)
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	f, err := r.c.Open(r.filename(h))
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			_ = f.Close()
			return nil, err
		}
	}

	if length > 0 {
		return backend.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (r *SFTP) Stat(h restic.Handle) (restic.FileInfo, error) {
	debug.Log("Stat(%v)", h)
	if err := r.clientError(); err != nil {
		return restic.FileInfo{}, err
	}

	if err := h.Valid(); err != nil {
		return restic.FileInfo{}, err
	}

	fi, err := r.c.Lstat(r.filename(h))
	if err != nil {
		return restic.FileInfo{}, errors.Wrap(err, "Lstat")
	}

	return restic.FileInfo{Size: fi.Size()}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (r *SFTP) Test(h restic.Handle) (bool, error) {
	debug.Log("Test(%v)", h)
	if err := r.clientError(); err != nil {
		return false, err
	}

	_, err := r.c.Lstat(r.filename(h))
	if os.IsNotExist(errors.Cause(err)) {
		return false, nil
	}

	if err != nil {
		return false, errors.Wrap(err, "Lstat")
	}

	return true, nil
}

// Remove removes the content stored at name.
func (r *SFTP) Remove(h restic.Handle) error {
	debug.Log("Remove(%v)", h)
	if err := r.clientError(); err != nil {
		return err
	}

	return r.c.Remove(r.filename(h))
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this. If the channel done is closed, sending
// stops.
func (r *SFTP) List(t restic.FileType, done <-chan struct{}) <-chan string {
	debug.Log("list all %v", t)
	ch := make(chan string)

	go func() {
		defer close(ch)

		if t == restic.DataFile {
			// read first level
			basedir := r.dirname(restic.Handle{Type: t})

			list1, err := r.c.ReadDir(basedir)
			if err != nil {
				return
			}

			dirs := make([]string, 0, len(list1))
			for _, d := range list1 {
				dirs = append(dirs, d.Name())
			}

			// read files
			for _, dir := range dirs {
				entries, err := r.c.ReadDir(Join(basedir, dir))
				if err != nil {
					continue
				}

				items := make([]string, 0, len(entries))
				for _, entry := range entries {
					items = append(items, entry.Name())
				}

				for _, file := range items {
					select {
					case ch <- file:
					case <-done:
						return
					}
				}
			}
		} else {
			entries, err := r.c.ReadDir(r.dirname(restic.Handle{Type: t}))
			if err != nil {
				return
			}

			items := make([]string, 0, len(entries))
			for _, entry := range entries {
				items = append(items, entry.Name())
			}

			for _, file := range items {
				select {
				case ch <- file:
				case <-done:
					return
				}
			}
		}
	}()

	return ch

}

var closeTimeout = 2 * time.Second

// Close closes the sftp connection and terminates the underlying command.
func (r *SFTP) Close() error {
	debug.Log("")
	if r == nil {
		return nil
	}

	err := r.c.Close()
	debug.Log("Close returned error %v", err)

	// wait for closeTimeout before killing the process
	select {
	case err := <-r.result:
		return err
	case <-time.After(closeTimeout):
	}

	if err := r.cmd.Process.Kill(); err != nil {
		return err
	}

	// get the error, but ignore it
	<-r.result
	return nil
}
