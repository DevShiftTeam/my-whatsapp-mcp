#!/bin/sh
# Experimental macOS arm64 native builds from existing source checkouts.
# This helper is separate from the keyword-only app build.
set -eu
umask 077
component=${1:-plan}
source_dir=${2:-}
output_dir=${3:-}
dependency_prefix=${4:-}
cmake_binary=${WA_MEDIA_CMAKE:-}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
case "$component" in
 plan)
  printf '%s\n' 'Usage: build-media-native.sh COMPONENT PINNED_SOURCE NEW_OUTPUT [DEPENDENCY_PREFIX]' 'Components: whisper.cpp | llama.cpp | ffmpeg | leptonica | tesseract' 'Experimental: macOS arm64 only. Uses existing source checkouts at pinned revisions.' 'Set WA_MEDIA_BUILD_ADMITTED=yes and WA_MEDIA_CMAKE to an absolute CMake path.' 'Leptonica installs under NEW_OUTPUT/prefix. Tesseract uses that static dependency prefix.' 'Validate outputs and third-party licenses separately before distributing a bundle.'
  exit 0;;
 whisper.cpp) revision=927cfce34f31707e17f2bff35c349632fb9e2c3a;target=whisper-cli;;
 llama.cpp) revision=4f5406761517648c23dbd60ea5ade37f77a316c9;target=llama-mtmd-cli;;
 ffmpeg) revision=894da5ca7d742e4429ffb2af534fcda0103ef593;;
 leptonica) revision=63aef18d98432b8582a1565e241f7bd2ee9cc8d9;target=leptonica;;
 tesseract) revision=3b7c70e34dea179549ed3e995872e2e019eb8477;target=libtesseract;;
 *) printf '%s\n' 'unsupported component' >&2;exit 2;;
esac
if [ "$(uname -s)" != Darwin ] || [ "$(uname -m)" != arm64 ]; then
 printf '%s\n' 'Native media build requires macOS arm64.' >&2;exit 2
fi
MACOSX_DEPLOYMENT_TARGET=14.0
export MACOSX_DEPLOYMENT_TARGET
case "$source_dir:$output_dir" in /*:/*) ;; *) printf '%s\n' 'absolute source/output required' >&2;exit 2;; esac
[ -d "$source_dir" ] && [ ! -L "$source_dir" ] && [ ! -e "$output_dir" ] || exit 2
[ "${WA_MEDIA_BUILD_ADMITTED:-}" = "yes" ] || { printf '%s\n' 'Set WA_MEDIA_BUILD_ADMITTED=yes to enable the experimental build.' >&2;exit 2; }
[ "$(git -C "$source_dir" rev-parse HEAD)" = "$revision" ] || exit 2
[ -z "$(git -C "$source_dir" status --porcelain)" ] || exit 2
if [ "$component" != ffmpeg ]; then
 case "$cmake_binary" in /*) ;; *) printf '%s\n' 'absolute WA_MEDIA_CMAKE path required' >&2;exit 2;; esac
 [ -f "$cmake_binary" ] && [ -x "$cmake_binary" ] && [ ! -L "$cmake_binary" ] || exit 2
fi
mkdir -m 700 "$output_dir"
if [ "$component" = llama.cpp ]; then
 git clone --local --no-hardlinks "$source_dir" "$output_dir/source"
 source_dir="$output_dir/source"
 git -C "$source_dir" apply --check "$repo_dir/packaging/media-worker-patches/llama-cpu-caption-v2.patch"
 git -C "$source_dir" apply "$repo_dir/packaging/media-worker-patches/llama-cpu-caption-v2.patch"
 git -C "$source_dir" apply --check "$repo_dir/packaging/media-worker-patches/llama-gguf-filename-rewind-v1.patch"
 git -C "$source_dir" apply "$repo_dir/packaging/media-worker-patches/llama-gguf-filename-rewind-v1.patch"
 # A fixed downstream target exercises the production formatter without models.
 mkdir -m 700 "$output_dir/project"
 cp "$script_dir/native-caption-policy-test.cpp" "$output_dir/project/native-caption-policy-test.cpp"
 cp "$script_dir/native-gguf-rewind-test.cpp" "$output_dir/project/native-gguf-rewind-test.cpp"
 tee "$output_dir/project/CMakeLists.txt" >/dev/null <<'CMAKE'
cmake_minimum_required(VERSION 3.14)
project(wa_caption_policy LANGUAGES C CXX)
set(LLAMA_BUILD_COMMON ON CACHE BOOL "" FORCE)
add_subdirectory("${CMAKE_CURRENT_SOURCE_DIR}/../source" llama)
add_executable(wa-caption-policy-test native-caption-policy-test.cpp)
target_link_libraries(wa-caption-policy-test PRIVATE llama-common)
set_target_properties(wa-caption-policy-test PROPERTIES RUNTIME_OUTPUT_DIRECTORY "${CMAKE_BINARY_DIR}/bin")
add_executable(wa-gguf-rewind-test native-gguf-rewind-test.cpp)
target_link_libraries(wa-gguf-rewind-test PRIVATE ggml-base)
set_target_properties(wa-gguf-rewind-test PROPERTIES CXX_STANDARD 17 RUNTIME_OUTPUT_DIRECTORY "${CMAKE_BINARY_DIR}/bin")
CMAKE
 source_dir="$output_dir/project"
fi
if [ "$component" = ffmpeg ]; then
 cd "$output_dir"
 "$source_dir/configure" --disable-autodetect --disable-everything --disable-network --disable-gpl --disable-nonfree --disable-doc --disable-debug --disable-programs --enable-ffmpeg --enable-avcodec --enable-avformat --enable-avutil --enable-swresample --enable-avfilter --enable-protocol=pipe --enable-demuxer=ogg,wav --enable-decoder=opus,vorbis,pcm_s16le --enable-parser=opus --enable-encoder=pcm_s16le --enable-muxer=pcm_s16le --enable-filter=aresample,aformat,anull --disable-shared --enable-static --extra-cflags=-mmacosx-version-min=14.0 --extra-ldflags=-mmacosx-version-min=14.0
 make -j2 ffmpeg
else
 # Ignore compiler search paths inherited from unrelated build environments.
 unset CC CXX CPP CFLAGS CXXFLAGS CPPFLAGS LDFLAGS CMAKE_PREFIX_PATH CMAKE_MODULE_PATH
 unset PKG_CONFIG_PATH PKG_CONFIG_PRELOADED_FILES PKG_CONFIG_SYSROOT_DIR PKG_CONFIG_TOP_BUILD_DIR PKG_CONFIG_DISABLE_UNINSTALLED PKG_CONFIG_LIBDIR
 set -- -DCMAKE_OSX_DEPLOYMENT_TARGET=14.0 -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DCMAKE_OSX_ARCHITECTURES=arm64 -DCMAKE_C_COMPILER=/usr/bin/clang -DCMAKE_CXX_COMPILER=/usr/bin/clang++ -DCMAKE_C_COMPILER_LAUNCHER= -DCMAKE_CXX_COMPILER_LAUNCHER= -DCMAKE_FIND_USE_PACKAGE_REGISTRY=OFF -DCMAKE_FIND_USE_SYSTEM_PACKAGE_REGISTRY=OFF
 case "$component" in
  whisper.cpp) set -- "$@" -DGGML_METAL=OFF -DGGML_CUDA=OFF -DGGML_VULKAN=OFF -DGGML_BLAS=OFF -DGGML_OPENMP=OFF -DGGML_NATIVE=OFF -DWHISPER_CURL=OFF -DWHISPER_BUILD_SERVER=OFF -DWHISPER_BUILD_TESTS=OFF;;
  llama.cpp)
   set -- "$@" -DLLAMA_USE_SYSTEM_GGML=OFF -DLLAMA_BUILD_COMMON=ON -DLLAMA_BUILD_TOOLS=ON -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF -DLLAMA_BUILD_SERVER=OFF -DLLAMA_BUILD_APP=OFF -DLLAMA_OPENSSL=OFF -DLLAMA_BUILD_BORINGSSL=OFF -DLLAMA_BUILD_LIBRESSL=OFF -DLLAMA_SUBPROCESS=OFF -DLLAMA_LLGUIDANCE=OFF -DMTMD_VIDEO=OFF -DGGML_CPU=ON -DGGML_BACKEND_DL=OFF -DGGML_CPU_ALL_VARIANTS=OFF -DGGML_NATIVE=OFF -DGGML_STATIC=OFF -DGGML_ACCELERATE=OFF -DGGML_BLAS=OFF -DGGML_OPENMP=OFF -DGGML_OPENMP_FETCH=OFF -DGGML_CPU_HBM=OFF -DGGML_CPU_KLEIDIAI=OFF -DGGML_CCACHE=OFF -DGGML_LLAMAFILE=ON
   for backend in CUDA MUSA HIP VULKAN WEBGPU ZDNN VIRTGPU VIRTGPU_BACKEND METAL RPC SYCL OPENVINO ET ET_SYSEMU OPENCL HEXAGON ZENDNN CANN; do set -- "$@" "-DGGML_${backend}=OFF"; done
   ;;
  leptonica)
   set -- "$@" "-DCMAKE_INSTALL_PREFIX=$output_dir/prefix" -DBUILD_PROG=OFF -DSW_BUILD=OFF -DSYM_LINK=OFF -DENABLE_ZLIB=OFF -DENABLE_PNG=OFF -DENABLE_GIF=OFF -DENABLE_JPEG=OFF -DENABLE_TIFF=OFF -DENABLE_WEBP=OFF -DENABLE_OPENJPEG=OFF
   ;;
  tesseract)
   case "$dependency_prefix" in /*) ;; *) exit 2;; esac
   [ -d "$dependency_prefix" ] && [ ! -L "$dependency_prefix" ] && [ -f "$dependency_prefix/lib/libleptonica.a" ] || exit 2
   set -- "$@" "-DLeptonica_DIR=$dependency_prefix/lib/cmake/leptonica" "-DCMAKE_FIND_ROOT_PATH=$dependency_prefix" -DCMAKE_FIND_ROOT_PATH_MODE_LIBRARY=ONLY -DCMAKE_FIND_ROOT_PATH_MODE_INCLUDE=ONLY -DCMAKE_FIND_ROOT_PATH_MODE_PACKAGE=ONLY -DCMAKE_FIND_ROOT_PATH_MODE_PROGRAM=NEVER -DCMAKE_DISABLE_FIND_PACKAGE_PkgConfig=ON -DSW_BUILD=OFF -DBUILD_TRAINING_TOOLS=OFF -DBUILD_TESTS=OFF -DOPENMP_BUILD=OFF -DGRAPHICS_DISABLED=ON -DDISABLED_LEGACY_ENGINE=ON -DENABLE_NATIVE=OFF -DENABLE_LTO=OFF -DDISABLE_TIFF=ON -DDISABLE_ARCHIVE=ON -DDISABLE_CURL=ON -DUSE_SYSTEM_ICU=OFF -DINSTALL_CONFIGS=OFF
   ;;
 esac
 "$cmake_binary" -S "$source_dir" -B "$output_dir/build" "$@"
 "$cmake_binary" --build "$output_dir/build" --parallel 2 --target "$target"
 if [ "$component" = tesseract ]; then
  /usr/bin/clang++ -mmacosx-version-min=14.0 -std=c++17 -O2 -arch arm64 -I"$source_dir/include" -I"$output_dir/build/include" -I"$dependency_prefix/include/leptonica" "$script_dir/native-media-ocr.cpp" "$output_dir/build/libtesseract.a" "$dependency_prefix/lib/libleptonica.a" -lm -pthread -o "$output_dir/wa-media-ocr"
 elif [ "$component" = leptonica ]; then
  "$cmake_binary" --install "$output_dir/build" --prefix "$output_dir/prefix"
 elif [ "$component" = llama.cpp ]; then
  "$cmake_binary" --build "$output_dir/build" --parallel 2 --target wa-caption-policy-test
  "$cmake_binary" --build "$output_dir/build" --parallel 2 --target wa-gguf-rewind-test
  printf '%s\n' 'Model-free caption and GGUF rewind tests built; run them separately.'
 fi
fi
printf '%s\n' 'Native build completed. Runtime, model quality, and redistribution validation are still required.'
