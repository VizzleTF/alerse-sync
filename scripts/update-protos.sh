#!/bin/sh
# Copies Chromium's sync protocol definitions into proto/ and regenerates the
# Go code. Run it after moving to a new Chromium version:
#
#	scripts/update-protos.sh ~/chromium/src
set -eu

src=${1:?usage: update-protos.sh <chromium src directory>}
root=$(cd "$(dirname "$0")/.." && pwd)

rm -f "$root"/proto/*.proto
cp "$src"/components/sync/protocol/*.proto "$root/proto/"
cp "$src/LICENSE" "$root/proto/LICENSE"
for file in "$root"/proto/*.proto; do
	sed -i \
		-e 's#^import "components/sync/protocol/#import "#' \
		-e 's#^package sync_pb;#package sync_pb;\noption go_package = "github.com/VizzleTF/arcomium-sync/internal/syncpb";#' \
		"$file"
done

cd "$root"
rm -f internal/syncpb/*.pb.go
protoc -I proto --go_out=. --go_opt=module=github.com/VizzleTF/arcomium-sync proto/*.proto
