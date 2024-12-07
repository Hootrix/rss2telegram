#!/bin/sh
# version=`date -u +"v%Y.%m%d"`
flags="-s -w -extldflags \"-static -fpic\" "
go build -ldflags "$flags" -o rss2telegram cmd/main.go
#&& upx -9 ./rss2telegram