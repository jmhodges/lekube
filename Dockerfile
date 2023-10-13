# Copyright 2021 Jeffrey M Hodges.
# SPDX-License-Identifier: Apache-2.0

FROM amd64/golang:1.21.3@sha256:d0214956a9c50c300e430c1f6c0a820007ace238e5242c53762e61b344659e05 as build

WORKDIR /go/src/github.com/jmhodges/lekube
ADD . /go/src/github.com/jmhodges/lekube

RUN go build -o /go/bin/lekube

# Now copy it into our base image.
FROM gcr.io/distroless/base-debian10@sha256:101798a3b76599762d3528635113f0466dc9655ecba82e8e33d410e2bf5cd319
COPY --from=build /go/bin/lekube /
CMD ["/lekube", "-conf", "/etc/lekube/lekube.json"]
