package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"golang.org/x/crypto/ssh/terminal"
	yaml "gopkg.in/yaml.v2"
)

// Config is a struct representation of the gantry config file
type Config struct {
	Dockerfile string   `yaml:"dockerfile"`
	Commands   []string `yaml:"commands"`
}

func main() {
	if len(os.Args) == 1 {
		if err := initialize(); err != nil {
			log.Fatal(err)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "run" {
		run()
	}
}

func initialize() error {
	fmt.Println("Initializing new gantry project")
	b, err := ioutil.ReadFile("./gantry.yml")
	if err != nil {
		return err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return err
	}
	if err := os.RemoveAll("./gantry/bin"); err != nil {
		return err
	}
	if err := os.MkdirAll("./gantry/bin", 0777); err != nil {
		return err
	}
	for _, command := range c.Commands {
		if err := ioutil.WriteFile(
			fmt.Sprintf("./gantry/bin/%s", command),
			[]byte(commandFile), 0755); err != nil {
			return err
		}
	}
	if err := ioutil.WriteFile("./gantry/activate", []byte(activateFile), 0755); err != nil {
		return err
	}
	return nil
}

func run() {
	fmt.Println("Running command with gantry")
	projRoot, err := filepath.Abs(filepath.Dir(os.Args[2]) + "../../../")
	if err != nil {
		log.Fatal(err)
	}
	b, err := ioutil.ReadFile(projRoot + "/gantry.yml")
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		log.Fatal(err)
	}
	if err := runContainer(projRoot, c); err != nil {
		log.Fatal(err)
	}
}

func runContainer(projRoot string, c Config) error {

	name := "gantry"
	ctx := context.Background()
	cli, err := client.NewEnvClient()
	if err != nil {
		return err
	}

	var createAndStart bool
	if i, err := cli.ContainerInspect(ctx, name); err != nil {
		log.Println(err)
		createAndStart = true
	} else {
		createAndStart = !i.State.Running
	}
	if createAndStart {
		fmt.Println("Building container for gantry")
		// consider not removing and resuming on a stopped container
		if err := cli.ContainerRemove(ctx, name, types.ContainerRemoveOptions{}); err != nil {
			log.Println(err)
		}

		buf := new(bytes.Buffer)
		if err := Tar(projRoot+"./"+c.Dockerfile, buf); err != nil {
			return err
		}

		imageBuildResponse, err := cli.ImageBuild(
			ctx,
			bytes.NewReader(buf.Bytes()),
			types.ImageBuildOptions{
				Dockerfile:     c.Dockerfile,
				Tags:           []string{name},
				Remove:         true,
				SuppressOutput: false})
		if err != nil {
			return err
		}
		io.Copy(os.Stdout, imageBuildResponse.Body)
		imageBuildResponse.Body.Close()

		resp, err := cli.ContainerCreate(ctx, &container.Config{
			Image: name,
			Cmd:   strslice.StrSlice{"sleep", "10000000"},
		}, &container.HostConfig{
			Binds: []string{"/Users/maxm/go/src/github.com/maxmcd/gantry/:/opt"},
		}, nil, "gantry")
		if err != nil {
			return err
		}

		if err := cli.ContainerStart(ctx, resp.ID, types.ContainerStartOptions{}); err != nil {
			return err
		}
	}

	cmd := make([]string, len(os.Args[2:]))
	copy(cmd, os.Args[2:])
	cmd[0] = filepath.Base(cmd[0])
	id, err := cli.ContainerExecCreate(ctx, name, types.ExecConfig{
		Tty:          true,
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          cmd,
	})
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	done := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		fmt.Println(sig)
		done <- true
	}()

	hr, err := cli.ContainerExecAttach(ctx, id.ID, types.ExecConfig{})
	if err != nil {
		return err
	}
	oldState, err := terminal.MakeRaw(0)
	if err != nil {
		return err
	}

	defer terminal.Restore(0, oldState)
	go func() {
		io.Copy(hr.Conn, os.Stdin)
		done <- true
	}()
	go func() {
		stdcopy.StdCopy(os.Stdout, os.Stderr, hr.Reader)
		done <- true
	}()
	<-done
	return nil
}

func Tar(src string, writers ...io.Writer) error {
	// ensure the src actually exists before trying to tar it
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("Unable to tar files - %v", err.Error())
	}
	mw := io.MultiWriter(writers...)
	gzw := gzip.NewWriter(mw)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	// https://medium.com/@skdomino/taring-untaring-files-in-go-6b07cf56bc07
	// walk path
	return filepath.Walk(src, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// create a new dir/file header
		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}
		// update the name to correctly reflect the desired destination when untaring
		header.Name = strings.TrimPrefix(strings.Replace(file, src, "", -1), string(filepath.Separator))

		// write the header
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		// return on non-regular files (thanks to [kumo](https://medium.com/@komuw/just-like-you-did-fbdd7df829d3) for this suggested update)
		if !fi.Mode().IsRegular() {
			return nil
		}
		// open files for taring
		f, err := os.Open(file)
		if err != nil {
			return err
		}
		// copy file data into tar writer
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		// manually close here after each file operation; defering would cause each file close
		// to wait until all operations have completed.
		f.Close()

		return nil
	})
}

var activateFile = `
# This file must be used with "source bin/activate" *from bash*
# you cannot run it directly
# (special thanks to virtualenv)

deactivate () {
    # reset old environment variables
    if [ -n "$_OLD_GANTRY_PATH" ] ; then
        PATH="$_OLD_GANTRY_PATH"
        export PATH
        unset _OLD_GANTRY_PATH
    fi

    # This should detect bash and zsh, which have a hash command that must
    # be called to get it to forget past commands.  Without forgetting
    # past commands the $PATH changes we made may not be respected
    if [ -n "$BASH" -o -n "$ZSH_VERSION" ] ; then
        hash -r 2>/dev/null
    fi

    if [ -n "$_OLD_GANTRY_PS1" ] ; then
        PS1="$_OLD_GANTRY_PS1"
        export PS1
        unset _OLD_GANTRY_PS1
    fi

    unset VIRTUAL_ENV
    if [ ! "$1" = "nondestructive" ] ; then
    # Self destruct!
        unset -f deactivate
    fi
}


# unset irrelavent variables
deactivate nondestructive

_OLD_GANTRY_PATH=$PATH
_OLD_GANTRY_PS1=$PS1


PATH="$(dirname ${BASH_SOURCE[0]})/bin:$PATH"
export PATH

PS1="(gantry) ${PS1:-}"
export PS1


# This should detect bash and zsh, which have a hash command that must
# be called to get it to forget past commands.  Without forgetting
# past commands the $PATH changes we made may not be respected
if [ -n "$BASH" -o -n "$ZSH_VERSION" ] ; then
    hash -r 2>/dev/null
fi
`

var commandFile = `
#!/bin/bash
set -e

which gantry

gantry run ${BASH_SOURCE[0]} $@
`
