#!/bin/bash
# scripts/build-static-zmq.sh

set -e

echo "Building static libsodium..."
wget -q https://download.libsodium.org/libsodium/releases/libsodium-1.0.18-stable.tar.gz
tar xzf libsodium-1.0.18-stable.tar.gz
cd libsodium-stable
./configure --prefix=/usr/local --enable-static --disable-shared --with-pic
make -j$(nproc)
make install
cd ..

echo "Building static libzmq..."
# Use fixed version for reproducible builds
git clone --depth=1 --branch v4.3.5 https://github.com/zeromq/libzmq
cd libzmq
mkdir build && cd build
cmake .. \
    -DCMAKE_INSTALL_PREFIX=/usr/local \
    -DBUILD_STATIC=ON \
    -DBUILD_SHARED=OFF \
    -DWITH_LIBSODIUM=ON \
    -DWITH_LIBSODIUM_STATIC=ON \
    -DCMAKE_PREFIX_PATH=/usr/local \
    -DCMAKE_BUILD_TYPE=Release
make -j$(nproc)
make install
cd ../..

echo "Static ZMQ build complete!"