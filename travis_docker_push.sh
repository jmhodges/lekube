#!/bin/bash

function die() {
  echo $1 > /dev/stderr
  exit 1
}

if [ -z "${TRAVIS_BRANCH}" ]; then
  die "not running in travis"
fi

if [[ ("${TRAVIS_BRANCH}" == "master" && "${TRAVIS_PULL_REQUEST}" == "false")|| "${TRAVIS_BRANCH}" =~ ^test_docker_push.* ]]; then
  echo "setting up push to docker"
else
  echo "not on pushable or deployable branch, so no docker work needed"
  exit
fi

REPO=jmhodges/lekube
SHA=$(git rev-parse --short HEAD)

docker login -e $DOCKER_EMAIL -u $DOCKER_USER -p $DOCKER_PASS || die "unable to login"

# DEPLOY_IMAGE is usually something like jmhodges/lekube:master-ffffff-48
# unless running on a test_docker_push branch
DEPLOY_IMAGE="$REPO:${TRAVIS_BUILD_NUMBER}-${TRAVIS_BRANCH}-${SHA}"

docker build -f Dockerfile -t ${DEPLOY_IMAGE} . || die "unable to build as ${DEPLOY_IMAGE}"

echo "Pushing image to docker hub: ${DEPLOY_IMAGE}"
docker push $REPO || die "unable to push docker tags"
echo "Pushed image to docker hub: ${DEPLOY_IMAGE}"
