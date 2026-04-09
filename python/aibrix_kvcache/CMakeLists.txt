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
project(aibrix_extensions LANGUAGES CXX CUDA)

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

# Find CUDA
find_package(CUDAToolkit REQUIRED)

# Ensure the 'nvcc' command is in the PATH
find_program(NVCC_EXECUTABLE nvcc REQUIRED)

message(STATUS "CUDAToolkit_VERSION: ${CUDAToolkit_VERSION}")

# Supported NVIDIA architectures.
if(CUDAToolkit_VERSION VERSION_GREATER_EQUAL 13.0)
  set(CUDA_SUPPORTED_ARCHS "7.5;8.0;8.6;8.7;8.9;9.0;10.0;11.0;12.0")
elseif(CUDAToolkit_VERSION VERSION_GREATER_EQUAL 12.8)
  set(CUDA_SUPPORTED_ARCHS "7.0;7.2;7.5;8.0;8.6;8.7;8.9;9.0;10.0;10.1;12.0")
else()
  set(CUDA_SUPPORTED_ARCHS "7.0;7.2;7.5;8.0;8.6;8.7;8.9;9.0")
endif()
message(STATUS "CUDA_SUPPORTED_ARCHS: ${CUDA_SUPPORTED_ARCHS}")

# Get target CUDA architectures
if(DEFINED ENV{AIBRIX_CUDA_ARCHES})
    set(CUDA_ARCHS "$ENV{AIBRIX_CUDA_ARCHES}")
    message(STATUS "Using CUDA architectures from AIBRIX_CUDA_ARCHES: ${CUDA_ARCHS}")
  else()
    # Auto-detect GPU architectures using nvidia-smi
    find_program(NVIDIA_SMI nvidia-smi)
    if(NVIDIA_SMI)
      execute_process(
        COMMAND "${NVIDIA_SMI}" --query-gpu=compute_cap --format=csv,noheader
        OUTPUT_VARIABLE _SMI_OUTPUT
        OUTPUT_STRIP_TRAILING_WHITESPACE
        ERROR_QUIET
      )
      if(_SMI_OUTPUT)
        set(_DETECTED_ARCHS "")
        string(REPLACE "\n" ";" _GPU_LINES "${_SMI_OUTPUT}")
        foreach(_LINE ${_GPU_LINES})
          if(_LINE MATCHES "^([0-9]+)\\.([0-9]+)")
            set(_MAJOR "${CMAKE_MATCH_1}")
            set(_MINOR "${CMAKE_MATCH_2}")
            list(APPEND _DETECTED_ARCHS "${_MAJOR}.${_MINOR}")
          endif()
        endforeach()
        if(_DETECTED_ARCHS)
          list(REMOVE_DUPLICATES _DETECTED_ARCHS)
          set(CUDA_ARCHS "${_DETECTED_ARCHS}")
          message(STATUS "Auto-detected CUDA architectures from GPUs: ${CUDA_ARCHS}")
        else()
          set(_USE_DEFAULT_ARCHS TRUE)
        endif()
      else()
        set(_USE_DEFAULT_ARCHS TRUE)
      endif()
    else()
      set(_USE_DEFAULT_ARCHS TRUE)
    endif()
    if(_USE_DEFAULT_ARCHS)
      set(CUDA_ARCHS "7.0;7.5;8.0;8.6;8.9;9.0")
      message(STATUS "Using default CUDA architectures: ${CUDA_ARCHS}")
    endif()
  endif()

# Filter the target architectures by the supported archs
set(_ORIGINAL_CUDA_ARCHS "${CUDA_ARCHS}")
cuda_archs_loose_intersection(CUDA_ARCHS
  "${CUDA_SUPPORTED_ARCHS}" "${CUDA_ARCHS}")
if(NOT CUDA_ARCHS)
  message(WARNING "No CUDA architectures match supported list, using: ${_ORIGINAL_CUDA_ARCHS}")
  set(CUDA_ARCHS "${_ORIGINAL_CUDA_ARCHS}")
endif()
message(STATUS "CUDA target architectures: ${CUDA_ARCHS}")

set(AIBRIX_GPU_FLAGS)

# Enable FP8 for CUDA 11.8+
if(CUDAToolkit_VERSION VERSION_GREATER_EQUAL 11.8)
  list(APPEND AIBRIX_GPU_FLAGS "-DENABLE_FP8")
endif()

# Set nvcc parallelism
if(NVCC_THREADS)
  list(APPEND AIBRIX_GPU_FLAGS "--threads=${NVCC_THREADS}")
endif()

#
# Use FetchContent for C++ dependencies that are compiled as part of AIBrix's build process.
#
include(FetchContent)
file(MAKE_DIRECTORY ${FETCHCONTENT_BASE_DIR}) # Ensure the directory exists
message(STATUS "FetchContent base directory: ${FETCHCONTENT_BASE_DIR}")

#
# _aibrix_C extension
#
set(AIBRIX_EXT_SRC
  "csrc/cache_kernels.cu")

message(STATUS "Enabling C extension.")

define_gpu_extension_target(
  _aibrix_C
  DESTINATION aibrix_kvcache
  SOURCES ${AIBRIX_EXT_SRC}
  COMPILE_FLAGS ${AIBRIX_GPU_FLAGS}
  ARCHITECTURES ${CUDA_ARCHS}
  USE_SABI 3
  WITH_SOABI)
