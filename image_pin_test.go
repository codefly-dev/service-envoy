package main

import (
	"strings"
	"testing"
)

func TestRuntimeImageIsImmutable(t *testing.T) {
	if runtimeImage.Digest == "" || strings.Contains(runtimeImage.Tag, "latest") {
		t.Fatalf("Envoy image is not pinned: %+v", runtimeImage)
	}
}
