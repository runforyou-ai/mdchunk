#!/bin/sh
# Runs every fuzz target in the module for the given duration (default 30s).
set -eu
duration="${1:-30s}"
for pkg in $(go list ./...); do
	for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
		echo "== $pkg $target"
		go test "$pkg" -run '^$' -fuzz "^${target}\$" -fuzztime "$duration"
	done
done
