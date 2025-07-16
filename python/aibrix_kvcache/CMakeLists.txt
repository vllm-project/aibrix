# Adapted from vLLM
cmake_minimum_required(VERSION 3.26)

# When building directly using CMake, make sure you run the install step
# (it places the .so files in the correct location).
#
# Example:
# mkdir build && cd build
# cmake -G Ninja -DVLLM_PYTHON_EXECUTABLE=`which python3` -DCMAKE_INSTALL_PREFIX=.. ..
# cmake --build . --target install
#
# If you want to only build one target, make sure to install it manually:
# cmake --build . --target _aibrix_C
# cmake --install . --component _aibrix_C
project(aibrix_extensions LANGUAGES CXX)

set(AIBRIX_TARGET_DEVICE "cuda" CACHE STRING "Target device backend")
message(STATUS "Build type: ${CMAKE_BUILD_TYPE}")
message(STATUS "Target device: ${AIBRIX_TARGET_DEVICE}")

include(${CMAKE_CURRENT_LIST_DIR}/cmake/utils.cmake)

# Suppress potential warnings about unused manually-specified variables
set(ignoreMe "${AIBRIX_PYTHON_PATH}")

# Prevent installation of dependencies (cutlass) by default.
install(CODE "set(CMAKE_INSTALL_LOCAL_ONLY TRUE)" ALL_COMPONENTS)

#
# Supported python versions.  These versions will be searched in order, the
# first match will be selected.  These should be kept in sync with setup.py.
#
set(PYTHON_SUPPORTED_VERSIONS "3.9" "3.10" "3.11" "3.12")

#
# Supported/expected torch versions for CUDA/ROCm.
#
# Currently, having an incorrect pytorch version results in a warning
# rather than an error.
#
# Note: the CUDA torch version is derived from pyproject.toml and various
# requirements.txt files and should be kept consistent.  The ROCm torch
# versions are derived from docker/Dockerfile.rocm
#
set(TORCH_SUPPORTED_VERSION_CUDA "2.7.0")
set(TORCH_SUPPORTED_VERSION_ROCM "2.7.0")

#
# Try to find python package with an executable that exactly matches
# `AIBRIX_PYTHON_EXECUTABLE` and is one of the supported versions.
#
if (AIBRIX_PYTHON_EXECUTABLE)
  find_python_from_executable(${AIBRIX_PYTHON_EXECUTABLE} "${PYTHON_SUPPORTED_VERSIONS}")
else()
  message(FATAL_ERROR
    "Please set AIBRIX_PYTHON_EXECUTABLE to the path of the desired python version"
    " before running cmake configure.")
endif()

#
# Update cmake's `CMAKE_PREFIX_PATH` with torch location.
#
append_cmake_prefix_path("torch" "torch.utils.cmake_prefix_path")

# Ensure the 'nvcc' command is in the PATH
find_program(NVCC_EXECUTABLE nvcc)
if (CUDA_FOUND AND NOT NVCC_EXECUTABLE)
    message(FATAL_ERROR "nvcc not found")
endif()

#
# Import torch cmake configuration.
# Torch also imports CUDA (and partially HIP) languages with some customizations,
# so there is no need to do this explicitly with check_language/enable_language,
# etc.
#
find_package(Torch REQUIRED)

# Supported NVIDIA architectures.
# This check must happen after find_package(Torch) because that's when CMAKE_CUDA_COMPILER_VERSION gets defined
if(DEFINED CMAKE_CUDA_COMPILER_VERSION AND
   CMAKE_CUDA_COMPILER_VERSION VERSION_GREATER_EQUAL 12.8)
  set(CUDA_SUPPORTED_ARCHS "7.0;7.2;7.5;8.0;8.6;8.7;8.9;9.0;10.0;10.1;12.0")
else()
  set(CUDA_SUPPORTED_ARCHS "7.0;7.2;7.5;8.0;8.6;8.7;8.9;9.0")
endif()

#
# Set up GPU language and check the torch version and warn if it isn't
# what is expected.
#
if (CUDA_FOUND)
  set(AIBRIX_GPU_LANG "CUDA")

  if (NOT Torch_VERSION VERSION_EQUAL ${TORCH_SUPPORTED_VERSION_CUDA})
    message(WARNING "Pytorch version ${TORCH_SUPPORTED_VERSION_CUDA} "
      "expected for CUDA build, saw ${Torch_VERSION} instead.")
  endif()
else()
  message(FATAL_ERROR "Can't find CUDA installation.")
endif()


if(AIBRIX_GPU_LANG STREQUAL "CUDA")
  #
  # For cuda we want to be able to control which architectures we compile for on
  # a per-file basis in order to cut down on compile time. So here we extract
  # the set of architectures we want to compile for and remove the from the
  # CMAKE_CUDA_FLAGS so that they are not applied globally.
  #
  clear_cuda_arches(CUDA_ARCH_FLAGS)
  extract_unique_cuda_archs_ascending(CUDA_ARCHS "${CUDA_ARCH_FLAGS}")
  message(STATUS "CUDA target architectures: ${CUDA_ARCHS}")
  # Filter the target architectures by the supported supported archs
  # since for some files we will build for all CUDA_ARCHS.
  cuda_archs_loose_intersection(CUDA_ARCHS
    "${CUDA_SUPPORTED_ARCHS}" "${CUDA_ARCHS}")
  message(STATUS "CUDA supported target architectures: ${CUDA_ARCHS}")
else()
  message(FATAL_ERROR "AIBrix ONLY supports CUDA.")
endif()

#
# Query torch for additional GPU compilation flags for the given
# `AIBRIX_GPU_LANG`.
# The final set of arches is stored in `AIBRIX_GPU_FLAGS`.
#
get_torch_gpu_compiler_flags(AIBRIX_GPU_FLAGS ${AIBRIX_GPU_LANG})

#
# Set nvcc parallelism.
#
if(NVCC_THREADS AND AIBRIX_GPU_LANG STREQUAL "CUDA")
  list(APPEND AIBRIX_GPU_FLAGS "--threads=${NVCC_THREADS}")
endif()


#
# Use FetchContent for C++ dependencies that are compiled as part of AIBrix's build process.
# setup.py will override FETCHCONTENT_BASE_DIR to play nicely with sccache.
# Each dependency that produces build artifacts should override its BINARY_DIR to avoid
# conflicts between build types. It should instead be set to ${CMAKE_BINARY_DIR}/<dependency>.
#
include(FetchContent)
file(MAKE_DIRECTORY ${FETCHCONTENT_BASE_DIR}) # Ensure the directory exists
message(STATUS "FetchContent base directory: ${FETCHCONTENT_BASE_DIR}")

#
# Define other extension targets
#

#
# _aibrix_C extension
#

set(AIBRIX_EXT_SRC
  "csrc/cache_kernels.cu"
  "csrc/torch_bindings.cpp")

message(STATUS "Enabling C extension.")
define_gpu_extension_target(
  _aibrix_C
  DESTINATION aibrix_kvcache
  LANGUAGE ${AIBRIX_GPU_LANG}
  SOURCES ${AIBRIX_EXT_SRC}
  COMPILE_FLAGS ${AIBRIX_GPU_FLAGS}
  ARCHITECTURES ${AIBRIX_GPU_ARCHES}
  USE_SABI 3
  WITH_SOABI)
