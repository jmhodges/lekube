# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

# When a new debian stable comes out, we have to update this and the distroless
# base image at the same time. Dependabot won't do it for us, owing to how it
# interprets tags as release histories not versions.
FROM golang:1.21.5-bookworm@sha256:a6b787c7f9046e3fdaa97bca1f76fd23ff4108f612de885e1af87e0dccc02f99 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
