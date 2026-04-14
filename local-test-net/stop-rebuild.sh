set -e

./stop.sh

export GENESIS_OVERRIDES_FILE="inference-chain/test_genesis_overrides.json"
export BLST_PORTABLE=1
export SET_LATEST=1
export DEVSHARD_VERSION="v0.2.11"
make -C ../. build-docker

make -C ../. devshardd-build
