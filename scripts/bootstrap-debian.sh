#!/usr/bin/env bash
#
# On a (recent enough) Debian/Ubuntu system, bootstraps a source Go install
# (with improved parallel build patches) and the cockroach repo.

set -euxo pipefail

GOVERSION="${GOVERSION-1.7.1}"

cd "$(dirname "${0}")"

sudo apt-get update -q && sudo apt-get install -q -y --no-install-recommends build-essential git gdb patch bzip2 docker.io

sudo adduser "${USER}" docker

mkdir -p ~/go-bootstrap
curl "https://storage.googleapis.com/golang/go${GOVERSION}.linux-amd64.tar.gz" | tar -C ~/go-bootstrap -xz --strip=1
curl "https://storage.googleapis.com/golang/go${GOVERSION}.src.tar.gz" | tar -C ~ -xz

# Apply the patch for the "major" go version (e.g. 1.6, 1.7).
GOPATCHVER=$(echo ${GOVERSION} | grep -o "^[0-9]\+\.[0-9]\+")
patch -p1 -d ../go < "parallelbuilds-go${GOPATCHVER}.patch"

(cd ~/go/src && GOROOT_BOOTSTRAP=~/go-bootstrap ./make.bash)

echo 'export GOPATH=${HOME}; export PATH=${HOME}/go/bin:${GOPATH}/bin:${PATH}' >> ~/.bashrc_go
echo '. ~/.bashrc_go' >> ~/.bashrc

. ~/.bashrc_go

go get -d github.com/cockroachdb/cockroach
