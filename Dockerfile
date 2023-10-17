# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

# When a new debian stable comes out, we have to update this and the distroless
# base image at the same time. Dependabot won't do it for us, owing to how it
# interprets tags as release histories not versions.
FROM golang:1.21.3-bookworm@sha256:20f9ab52a8c3598ef13f86c9bc3b94db400e13df115ecacd8c3dfd0c0f060208 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian12
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
