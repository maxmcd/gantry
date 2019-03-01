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
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/strslice"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	yaml "gopkg.in/yaml.v2"
)

type Config struct {
	Dockerfile string   `yaml:"dockerfile"`
	Commands   []string `yaml:"commands"`
}

func main() {
	fmt.Println(os.Args)

	if len(os.Args) > 1 && os.Args[1] == "run" {
		projRoot, err := filepath.Abs(filepath.Dir(os.Args[2]) + "../../../")
		if err != nil {
			log.Fatal(err)
		}
		b, err := ioutil.ReadFile(projRoot + "/gantry.yml")
		var c Config
		if err := yaml.Unmarshal(b, &c); err != nil {
			log.Fatal(err)
		}
		exec.Command("docker", "")
		if err := runContainer(projRoot, c); err != nil {
			log.Fatal(err)
		}
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
		// consider not removing and resuming on a stopped container
		if err := cli.ContainerRemove(ctx, name, types.ContainerRemoveOptions{}); err != nil {
			log.Println(err)
		}

		buf := new(bytes.Buffer)
		if err := Tar(projRoot, buf); err != nil {
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

	id, err := cli.ContainerExecCreate(ctx, name, types.ExecConfig{
		Tty:          true,
		AttachStdin:  true,
		AttachStderr: true,
		AttachStdout: true,
		Cmd:          []string{"python", "./main.py"},
	})
	if err != nil {
		return err
	}

	hr, err := cli.ContainerExecAttach(ctx, id.ID, types.ExecConfig{})
	fmt.Println(hr, err)
	go io.Copy(hr.Conn, os.Stdin)
	stdcopy.StdCopy(os.Stdout, os.Stderr, hr.Reader)

	return nil
}

// https://medium.com/@skdomino/taring-untaring-files-in-go-6b07cf56bc07
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
