package logging

import (
	"testing"

	"github.com/caarlos0/log"
	"github.com/dnonakolesax/goreleaser/v2/pkg/context"
	"github.com/stretchr/testify/require"
)

func TestLogging(t *testing.T) {
	require.NoError(t, Log("foo", func(_ *context.Context) error {
		return nil
	})(nil))

	require.NoError(t, PadLog("foo", func(_ *context.Context) error {
		log.Info("a")
		return nil
	})(nil))
}
