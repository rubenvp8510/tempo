package util

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDistinctStringCollector(t *testing.T) {
	d := NewDistinctStringCollector(10)

	d.Collect("123")
	d.Collect("4567")
	d.Collect("890")
	d.Collect("11")

	require.True(t, d.Exceeded())
	require.Equal(t, []string{"123", "4567", "890"}, d.Strings())
}
