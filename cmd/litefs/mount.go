// go:build linux
package main

import (
	"context"
	"expvar"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mattn/go-shellwords"
	"github.com/superfly/litefs"
	"github.com/superfly/litefs/consul"
	"github.com/superfly/litefs/fuse"
	"github.com/superfly/litefs/http"
)

// MountCommand represents a command to mount the file system.
type MountCommand struct {
	cmd    *exec.Cmd  // subcommand
	execCh chan error // subcommand error channel

	Config Config

	Store      *litefs.Store
	Leaser     litefs.Leaser
	FileSystem *fuse.FileSystem
	HTTPServer *http.Server

	// Used for generating the advertise URL for testing.
	AdvertiseURLFn func() string
}

// NewMountCommand returns a new instance of MountCommand.
func NewMountCommand() *MountCommand {
	return &MountCommand{
		execCh: make(chan error),
		Config: NewConfig(),
	}
}

// ParseFlags parses the command line flags & config file.
func (c *MountCommand) ParseFlags(ctx context.Context, args []string) (err error) {
	// Split the args list if there is a double dash arg included. Arguments
	// after the double dash are used as the "exec" subprocess config option.
	args0, args1 := splitArgs(args)

	fs := flag.NewFlagSet("litefs-mount", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file path")
	noExpandEnv := fs.Bool("no-expand-env", false, "do not expand env vars in config")
	fs.Usage = func() {
		fmt.Println(`
The mount command will mount a LiteFS directory via FUSE and begin communicating
with the LiteFS cluster. The mount will be accessible once the node becomes the
primary or is able to connect and sync with the primary.

All options are specified in the litefs.yml config file which is searched for in
the present working directory, the current user's home directory, and then
finally at /etc/litefs.yml.

Usage:

	litefs mount [arguments]

Arguments:
`[1:])
		fs.PrintDefaults()
		fmt.Println("")
	}
	if err := fs.Parse(args0); err != nil {
		return err
	} else if fs.NArg() > 0 {
		return fmt.Errorf("too many arguments, specify a '--' to specify an exec command")
	}

	if err := c.parseConfig(ctx, *configPath, !*noExpandEnv); err != nil {
		return err
	}

	// Override "exec" field if specified on the CLI.
	if args1 != nil {
		c.Config.Exec = strings.Join(args1, " ")
	}

	return nil
}

// parseConfig parses the configuration file from configPath, if specified.
// Otherwise searches the standard list of search paths. Returns an error if
// no configuration files could be found.
func (c *MountCommand) parseConfig(ctx context.Context, configPath string, expandEnv bool) (err error) {
	// Only read from explicit path, if specified. Report any error.
	if configPath != "" {
		return ReadConfigFile(&c.Config, configPath, expandEnv)
	}

	// Otherwise attempt to read each config path until we succeed.
	for _, path := range configSearchPaths() {
		if path, err = filepath.Abs(path); err != nil {
			return err
		}

		if err := ReadConfigFile(&c.Config, path, expandEnv); err == nil {
			fmt.Printf("config file read from %s\n", path)
			return nil
		} else if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("cannot read config file at %s: %s", path, err)
		}
	}
	return fmt.Errorf("config file not found")
}

// Validate validates the application's configuration.
func (c *MountCommand) Validate(ctx context.Context) (err error) {
	if c.Config.MountDir == "" {
		return fmt.Errorf("mount directory required")
	} else if c.Config.DataDir == "" {
		return fmt.Errorf("data directory required")
	} else if c.Config.MountDir == c.Config.DataDir {
		return fmt.Errorf("mount directory and data directory cannot be the same path")
	}

	// Enforce exactly one lease mode.
	if c.Config.Consul != nil && c.Config.Static != nil {
		return fmt.Errorf("cannot specify both 'consul' and 'static' lease modes")
	} else if c.Config.Consul == nil && c.Config.Static == nil {
		return fmt.Errorf("must specify a lease mode ('consul', 'static')")
	}

	return nil
}

// configSearchPaths returns paths to search for the config file. It starts with
// the current directory, then home directory, if available. And finally it tries
// to read from the /etc directory.
func configSearchPaths() []string {
	a := []string{"litefs.yml"}
	if u, _ := user.Current(); u != nil && u.HomeDir != "" {
		a = append(a, filepath.Join(u.HomeDir, "litefs.yml"))
	}
	a = append(a, "/etc/litefs.yml")
	return a
}

func (c *MountCommand) Close() (err error) {
	if c.HTTPServer != nil {
		if e := c.HTTPServer.Close(); err == nil {
			err = e
		}
	}

	if c.FileSystem != nil {
		if e := c.FileSystem.Unmount(); err == nil {
			err = e
		}
	}

	if c.Store != nil {
		if e := c.Store.Close(); err == nil {
			err = e
		}
	}

	return err
}

func (c *MountCommand) Run(ctx context.Context) (err error) {
	fmt.Println(VersionString())

	// Start listening on HTTP server first so we can determine the URL.
	if err := c.initStore(ctx); err != nil {
		return fmt.Errorf("cannot init store: %w", err)
	} else if err := c.initHTTPServer(ctx); err != nil {
		return fmt.Errorf("cannot init http server: %w", err)
	}

	// Instantiate leaser.
	if c.Config.Consul != nil {
		log.Println("Using Consul to determine primary")
		if err := c.initConsul(ctx); err != nil {
			return fmt.Errorf("cannot init consul: %w", err)
		}
	} else { // static
		log.Printf("Using static primary: is-primary=%v hostname=%s advertise-url=%s", c.Config.Static.Primary, c.Config.Static.Hostname, c.Config.Static.AdvertiseURL)
		c.Leaser = litefs.NewStaticLeaser(c.Config.Static.Primary, c.Config.Static.Hostname, c.Config.Static.AdvertiseURL)
	}

	if err := c.openStore(ctx); err != nil {
		return fmt.Errorf("cannot open store: %w", err)
	}

	if err := c.initFileSystem(ctx); err != nil {
		return fmt.Errorf("cannot init file system: %w", err)
	}
	log.Printf("LiteFS mounted to: %s", c.FileSystem.Path())

	c.HTTPServer.Serve()
	log.Printf("http server listening on: %s", c.HTTPServer.URL())

	// Wait until the store either becomes primary or connects to the primary.
	log.Printf("waiting to connect to cluster")
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Store.ReadyCh():
		log.Printf("connected to cluster, ready")
	}

	// Execute subcommand, if specified in config.
	if err := c.execCmd(ctx); err != nil {
		return fmt.Errorf("cannot exec: %w", err)
	}

	return nil
}

func (c *MountCommand) initConsul(ctx context.Context) (err error) {
	// TEMP: Allow non-localhost addresses.

	// Use hostname from OS, if not specified.
	hostname := c.Config.Consul.Hostname
	if hostname == "" {
		if hostname, err = os.Hostname(); err != nil {
			return err
		}
	}

	// Determine the advertise URL for the LiteFS API.
	// Default to use the hostname and HTTP port. Also allow injection for tests.
	advertiseURL := c.Config.Consul.AdvertiseURL
	if c.AdvertiseURLFn != nil {
		advertiseURL = c.AdvertiseURLFn()
	}
	if advertiseURL == "" && hostname != "" {
		advertiseURL = fmt.Sprintf("http://%s:%d", hostname, c.HTTPServer.Port())
	}

	leaser := consul.NewLeaser(c.Config.Consul.URL, hostname, advertiseURL)
	if v := c.Config.Consul.Key; v != "" {
		leaser.Key = v
	}
	if v := c.Config.Consul.TTL; v > 0 {
		leaser.TTL = v
	}
	if v := c.Config.Consul.LockDelay; v > 0 {
		leaser.LockDelay = v
	}
	if err := leaser.Open(); err != nil {
		return fmt.Errorf("cannot connect to consul: %w", err)
	}
	log.Printf("initializing consul: key=%s url=%s hostname=%s advertise-url=%s", c.Config.Consul.Key, c.Config.Consul.URL, hostname, advertiseURL)

	c.Leaser = leaser
	return nil
}

func (c *MountCommand) initStore(ctx context.Context) error {
	c.Store = litefs.NewStore(c.Config.DataDir, c.Config.Candidate)
	c.Store.Debug = c.Config.Debug
	c.Store.StrictVerify = c.Config.StrictVerify
	c.Store.RetentionDuration = c.Config.Retention.Duration
	c.Store.RetentionMonitorInterval = c.Config.Retention.MonitorInterval
	c.Store.Client = http.NewClient()
	return nil
}

func (c *MountCommand) openStore(ctx context.Context) error {
	c.Store.Leaser = c.Leaser
	if err := c.Store.Open(); err != nil {
		return err
	}

	// Register expvar variable once so it doesn't panic during tests.
	expvarOnce.Do(func() { expvar.Publish("store", (*litefs.StoreVar)(c.Store)) })

	return nil
}

func (c *MountCommand) initFileSystem(ctx context.Context) error {
	// Build the file system to interact with the store.
	fsys := fuse.NewFileSystem(c.Config.MountDir, c.Store)
	if err := fsys.Mount(); err != nil {
		return fmt.Errorf("cannot open file system: %s", err)
	}

	// Attach file system to store so it can invalidate the page cache.
	c.Store.Invalidator = fsys

	c.FileSystem = fsys
	return nil
}

func (c *MountCommand) initHTTPServer(ctx context.Context) error {
	server := http.NewServer(c.Store, c.Config.HTTP.Addr)
	if err := server.Listen(); err != nil {
		return fmt.Errorf("cannot open http server: %w", err)
	}
	c.HTTPServer = server
	return nil
}

func (c *MountCommand) execCmd(ctx context.Context) error {
	// Exit if no subcommand specified.
	if c.Config.Exec == "" {
		return nil
	}

	// Execute subcommand process.
	args, err := shellwords.Parse(c.Config.Exec)
	if err != nil {
		return fmt.Errorf("cannot parse exec command: %w", err)
	}

	log.Printf("starting subprocess: %s %v", args[0], args[1:])

	c.cmd = exec.CommandContext(ctx, args[0], args[1:]...)
	c.cmd.Env = os.Environ()
	c.cmd.Stdout = os.Stdout
	c.cmd.Stderr = os.Stderr
	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("cannot start exec command: %w", err)
	}
	go func() { c.execCh <- c.cmd.Wait() }()

	return nil
}

var expvarOnce sync.Once