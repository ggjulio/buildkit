package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/containerd/console"
	"github.com/moby/buildkit/client"
	dockerfile "github.com/moby/buildkit/frontend/dockerfile/builder"
	"github.com/moby/buildkit/util/appcontext"
	"github.com/moby/buildkit/util/appdefaults"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"golang.org/x/sync/errgroup"
)

func main() {
	app := cli.NewApp()
	app.Name = "build-using-dockerfile"
	app.UsageText = `build-using-dockerfile [OPTIONS] PATH | URL | -`
	app.Description = `
build using Dockerfile.

This command mimics behavior of "docker build" command so that people can easily get started with BuildKit.
This command is NOT the replacement of "docker build", and should NOT be used for building production images.

By default, the built image is loaded to Docker.
`
	dockerIncompatibleFlags := []cli.Flag{
		cli.StringFlag{
			Name:   "buildkit-addr",
			Usage:  "buildkit daemon address",
			EnvVar: "BUILDKIT_HOST",
			Value:  appdefaults.Address,
		},
		cli.BoolFlag{
			Name:   "clientside-frontend",
			Usage:  "run dockerfile frontend client side, rather than builtin to buildkitd",
			EnvVar: "BUILDKIT_CLIENTSIDE_FRONTEND",
		},
	}
	app.Flags = append([]cli.Flag{
		cli.StringSliceFlag{
			Name:  "build-arg",
			Usage: "Set build-time variables",
		},
		cli.StringFlag{
			Name:  "file, f",
			Usage: "Name of the Dockerfile (Default is 'PATH/Dockerfile')",
		},
		cli.StringFlag{
			Name:  "tag, t",
			Usage: "Name and optionally a tag in the 'name:tag' format",
		},
		cli.StringFlag{
			Name:  "target",
			Usage: "Set the target build stage to build.",
		},
		cli.BoolFlag{
			Name:  "no-cache",
			Usage: "Do not use cache when building the image",
		},
	}, dockerIncompatibleFlags...)
	app.Action = action
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func action(clicontext *cli.Context) error {
	ctx := appcontext.Context()

	if tag := clicontext.String("tag"); tag == "" {
		return errors.New("tag is not specified")
	}
	c, err := client.New(ctx, clicontext.String("buildkit-addr"), client.WithFailFast())
	if err != nil {
		return err
	}
	pipeR, pipeW := io.Pipe()
	solveOpt, err := newSolveOpt(clicontext, pipeW)
	if err != nil {
		return err
	}
	ch := make(chan *client.SolveStatus)
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		var err error
		if clicontext.Bool("clientside-frontend") {
			_, err = c.Build(ctx, *solveOpt, "", dockerfile.Build, ch)
		} else {
			_, err = c.Solve(ctx, nil, *solveOpt, ch)
		}
		return err
	})
	eg.Go(func() error {
		var c console.Console
		if cn, err := console.ConsoleFromFile(os.Stderr); err == nil {
			c = cn
		}
		// not using shared context to not disrupt display but let is finish reporting errors
		_, err = progressui.DisplaySolveStatus(context.TODO(), c, os.Stdout, ch)
		return err
	})
	eg.Go(func() error {
		if err := loadDockerTar(pipeR); err != nil {
			return err
		}
		return pipeR.Close()
	})
	if err := eg.Wait(); err != nil {
		return err
	}
	logrus.Infof("Loaded the image %q to Docker.", clicontext.String("tag"))
	return nil
}

func newSolveOpt(clicontext *cli.Context, w io.WriteCloser) (*client.SolveOpt, error) {
	buildCtx := clicontext.Args().First()
	if buildCtx == "" {
		return nil, errors.New("please specify build context (e.g. \".\" for the current directory)")
	} else if buildCtx == "-" {
		return nil, errors.New("stdin not supported yet")
	}

	file := clicontext.String("file")
	if file == "" {
		file = filepath.Join(buildCtx, "Dockerfile")
	}
	localDirs := map[string]string{
		"context":    buildCtx,
		"dockerfile": filepath.Dir(file),
	}

	frontend := "dockerfile.v0" // TODO: use gateway
	if clicontext.Bool("clientside-frontend") {
		frontend = ""
	}
	frontendAttrs := map[string]string{
		"filename": filepath.Base(file),
	}
	if target := clicontext.String("target"); target != "" {
		frontendAttrs["target"] = target
	}
	if clicontext.Bool("no-cache") {
		frontendAttrs["no-cache"] = ""
	}
	for _, buildArg := range clicontext.StringSlice("build-arg") {
		kv := strings.SplitN(buildArg, "=", 2)
		if len(kv) != 2 {
			return nil, errors.Errorf("invalid build-arg value %s", buildArg)
		}
		frontendAttrs["build-arg:"+kv[0]] = kv[1]
	}
	return &client.SolveOpt{
		Exports: []client.ExportEntry{
			{
				Type: "docker", // TODO: use containerd image store when it is integrated to Docker
				Attrs: map[string]string{
					"name": clicontext.String("tag"),
				},
				Output: func(_ map[string]string) (io.WriteCloser, error) {
					return w, nil
				},
			},
		},
		LocalDirs:     localDirs,
		Frontend:      frontend,
		FrontendAttrs: frontendAttrs,
	}, nil
}

func loadDockerTar(r io.Reader) error {
	// no need to use moby/moby/client here
	cmd := exec.Command("docker", "load")
	cmd.Stdin = r
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
