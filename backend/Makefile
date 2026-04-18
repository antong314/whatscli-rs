# Simple Makefile for go

LLAMA_DIR := $(CURDIR)/_llama-go
WHISPER_DIR := $(CURDIR)/_whisper
WHISPER_LIB_DIR := $(WHISPER_DIR)/lib
OPUS_DIR := $(CURDIR)/_opus
export CGO_ENABLED=1

# hraban/opus uses pkg-config; we override with explicit flags pointing at our
# vendored static libraries so no system-level install is required.
OPUS_CGO_CFLAGS := -I$(OPUS_DIR)/include
OPUS_CGO_LDFLAGS := -L$(OPUS_DIR)/lib -lopusfile -lopus -logg -lm

build: deps
	CGO_CFLAGS="$(OPUS_CGO_CFLAGS)" CGO_LDFLAGS="$(OPUS_CGO_LDFLAGS)" \
	LIBRARY_PATH="$(LLAMA_DIR):$(WHISPER_LIB_DIR)" \
	C_INCLUDE_PATH="$(LLAMA_DIR):$(WHISPER_DIR)/include:$(WHISPER_DIR)/ggml/include" \
	go build -o whatscli

build-server: deps
	CGO_CFLAGS="$(OPUS_CGO_CFLAGS)" CGO_LDFLAGS="$(OPUS_CGO_LDFLAGS)" \
	LIBRARY_PATH="$(LLAMA_DIR):$(WHISPER_LIB_DIR)" \
	C_INCLUDE_PATH="$(LLAMA_DIR):$(WHISPER_DIR)/include:$(WHISPER_DIR)/ggml/include" \
	go build -o whatscli-server ./cmd/server/

clean:
	go clean
	rm -f whatscli whatscli-server

run: build
	./whatscli

run-server: build-server
	./whatscli-server

install: deps
	CGO_CFLAGS="$(OPUS_CGO_CFLAGS)" CGO_LDFLAGS="$(OPUS_CGO_LDFLAGS)" \
	LIBRARY_PATH="$(LLAMA_DIR):$(WHISPER_LIB_DIR)" \
	C_INCLUDE_PATH="$(LLAMA_DIR):$(WHISPER_DIR)/include:$(WHISPER_DIR)/ggml/include" \
	go install .

get:
	go get

update:
	go get -u

release:
	./release.sh

# Ensure all native dependencies are built before compiling Go code.
deps: llama whisper opus

# Build the llama-go native library for embedded inference.
llama:
	@if [ ! -d "$(LLAMA_DIR)" ]; then \
		echo "Cloning llama-go with submodules..."; \
		git clone --recurse-submodules --depth 1 https://github.com/tcpipuk/llama-go.git $(LLAMA_DIR); \
	fi
	@if [ ! -f "$(LLAMA_DIR)/libbinding.a" ]; then \
		cd "$(LLAMA_DIR)" && rm -rf build && mkdir build && \
		cd build && cmake ../llama.cpp -DLLAMA_CURL=OFF -DBUILD_SHARED_LIBS=OFF -DGGML_METAL=ON -DGGML_BLAS=ON -DGGML_BLAS_VENDOR=Apple && \
		cmake --build . --config Release -j$$(sysctl -n hw.ncpu 2>/dev/null || nproc) && \
		cd "$(LLAMA_DIR)" && \
		cp build/src/libllama.a build/ggml/src/libggml.a build/ggml/src/libggml-base.a \
		   build/ggml/src/libggml-cpu.a build/ggml/src/ggml-metal/libggml-metal.a \
		   build/ggml/src/ggml-blas/libggml-blas.a build/common/libcommon.a . && \
		c++ -std=c++17 -O3 -DNDEBUG \
		   -I./llama.cpp -I./ -I./llama.cpp/ggml/include -I./llama.cpp/include \
		   -I./llama.cpp/common -I./llama.cpp/vendor \
		   -c wrapper.cpp -o wrapper.o && \
		ar crs libbinding.a wrapper.o; \
	else \
		echo "libbinding.a already exists, skipping build"; \
	fi

# Build the whisper.cpp native library for speech-to-text.
whisper:
	@if [ ! -d "$(WHISPER_DIR)" ]; then \
		echo "Cloning whisper.cpp..."; \
		git clone --depth 1 https://github.com/ggerganov/whisper.cpp.git $(WHISPER_DIR); \
	fi
	@if [ ! -f "$(WHISPER_LIB_DIR)/libwhisper.a" ]; then \
		cd "$(WHISPER_DIR)" && rm -rf build && mkdir -p build lib && \
		cd build && cmake .. -DBUILD_SHARED_LIBS=OFF -DGGML_METAL=ON -DGGML_BLAS=ON \
			-DGGML_BLAS_VENDOR=Apple -DWHISPER_BUILD_EXAMPLES=OFF -DWHISPER_BUILD_TESTS=OFF \
			-DCMAKE_BUILD_TYPE=Release && \
		cmake --build . --config Release -j$$(sysctl -n hw.ncpu 2>/dev/null || nproc) && \
		cd "$(WHISPER_DIR)" && \
		cp build/src/libwhisper.a build/ggml/src/libggml.a build/ggml/src/libggml-base.a \
		   build/ggml/src/libggml-cpu.a build/ggml/src/ggml-metal/libggml-metal.a \
		   build/ggml/src/ggml-blas/libggml-blas.a lib/; \
	else \
		echo "libwhisper.a already exists, skipping build"; \
	fi

# Build libogg, libopus, and libopusfile from source for OGG/Opus decoding.
opus:
	@if [ ! -f "$(OPUS_DIR)/lib/libopusfile.a" ]; then \
		mkdir -p "$(OPUS_DIR)/src" "$(OPUS_DIR)/lib" "$(OPUS_DIR)/include/opus" "$(OPUS_DIR)/include/ogg" && \
		echo "Building libogg..." && \
		cd "$(OPUS_DIR)/src" && \
		if [ ! -d libogg ]; then git clone --depth 1 https://github.com/xiph/ogg.git libogg; fi && \
		cd libogg && mkdir -p build && cd build && \
		cmake .. -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTING=OFF \
			-DCMAKE_INSTALL_PREFIX="$(OPUS_DIR)/install" && \
		cmake --build . --config Release -j$$(sysctl -n hw.ncpu 2>/dev/null || nproc) && \
		cmake --install . && \
		echo "Building libopus..." && \
		cd "$(OPUS_DIR)/src" && \
		if [ ! -d opus ]; then git clone --depth 1 https://github.com/xiph/opus.git opus; fi && \
		cd opus && mkdir -p build && cd build && \
		cmake .. -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DBUILD_TESTING=OFF \
			-DCMAKE_INSTALL_PREFIX="$(OPUS_DIR)/install" && \
		cmake --build . --config Release -j$$(sysctl -n hw.ncpu 2>/dev/null || nproc) && \
		cmake --install . && \
		echo "Building libopusfile..." && \
		cd "$(OPUS_DIR)/src" && \
		if [ ! -d opusfile ]; then git clone --depth 1 https://github.com/xiph/opusfile.git opusfile; fi && \
		cd opusfile && mkdir -p build && cd build && \
		cmake .. -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
			-DCMAKE_INSTALL_PREFIX="$(OPUS_DIR)/install" \
			-DCMAKE_PREFIX_PATH="$(OPUS_DIR)/install" \
			-DOP_DISABLE_HTTP=ON -DOP_DISABLE_DOCS=ON -DOP_DISABLE_EXAMPLES=ON && \
		cmake --build . --config Release -j$$(sysctl -n hw.ncpu 2>/dev/null || nproc) && \
		cmake --install . && \
		cp "$(OPUS_DIR)/install/lib/libogg.a" "$(OPUS_DIR)/install/lib/libopus.a" \
		   "$(OPUS_DIR)/install/lib/libopusfile.a" "$(OPUS_DIR)/lib/" && \
		cp "$(OPUS_DIR)/install/include/ogg/"*.h "$(OPUS_DIR)/include/ogg/" && \
		cp "$(OPUS_DIR)/install/include/opus/"*.h "$(OPUS_DIR)/include/opus/"; \
	else \
		echo "libopusfile.a already exists, skipping build"; \
	fi

.PHONY: build build-server clean run run-server install get update release deps llama whisper opus
