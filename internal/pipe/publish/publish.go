// Package publish contains the publishing pipe.
package publish

import (
	"fmt"

	"github.com/dnonakolesax/goreleaser/v2/internal/middleware/errhandler"
	"github.com/dnonakolesax/goreleaser/v2/internal/middleware/logging"
	"github.com/dnonakolesax/goreleaser/v2/internal/middleware/skip"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/artifactory"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/aur"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/aursources"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/blob"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/brew"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/cask"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/chocolatey"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/custompublishers"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/docker"
	dockerv2 "github.com/dnonakolesax/goreleaser/v2/internal/pipe/docker/v2"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/dockerdigest"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/ko"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/krew"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/mcp"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/milestone"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/nix"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/release"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/scoop"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/sign"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/snapcraft"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/upload"
	"github.com/dnonakolesax/goreleaser/v2/internal/pipe/winget"
	"github.com/dnonakolesax/goreleaser/v2/internal/skips"
	"github.com/dnonakolesax/goreleaser/v2/pkg/context"
)

// Publisher should be implemented by pipes that want to publish artifacts.
type Publisher interface {
	fmt.Stringer

	// Default sets the configuration defaults
	Publish(ctx *context.Context) error
}

// New publish pipeline.
func New() Pipe {
	return Pipe{
		pipeline: []Publisher{
			blob.Pipe{},
			upload.Pipe{},
			artifactory.Pipe{},
			docker.Pipe{},
			docker.ManifestPipe{},
			dockerv2.Publish{},
			dockerdigest.Pipe{},
			ko.Pipe{},
			sign.DockerPipe{},
			snapcraft.Pipe{},
			// This should be one of the last steps
			release.Pipe{},
			// brew et al use the release URL, so, they should be last
			nix.New(),
			winget.Pipe{},
			brew.Pipe{},
			cask.Pipe{},
			aur.Pipe{},
			aursources.Pipe{},
			krew.Pipe{},
			scoop.Pipe{},
			chocolatey.Pipe{},
			mcp.New(),
			milestone.Pipe{},
			custompublishers.Pipe{},
		},
	}
}

// Pipe that publishes artifacts.
type Pipe struct {
	pipeline []Publisher
}

func (Pipe) String() string                 { return "publishing" }
func (Pipe) Skip(ctx *context.Context) bool { return skips.Any(ctx, skips.Publish) }

func (p Pipe) Run(ctx *context.Context) error {
	memo := errhandler.Memo{}
	for _, publisher := range p.pipeline {
		if err := skip.Maybe(
			publisher,
			logging.PadLog(
				publisher.String(),
				errhandler.Handle(publisher.Publish),
			),
		)(ctx); err != nil {
			if ig, ok := publisher.(Continuable); ok && ig.ContinueOnError() && !ctx.FailFast {
				memo.Memorize(fmt.Errorf("%s: %w", publisher.String(), err))
				continue
			}
			return fmt.Errorf("%s: failed to publish artifacts: %w", publisher.String(), err)
		}
	}
	return memo.Error()
}

type Continuable interface {
	ContinueOnError() bool
}
