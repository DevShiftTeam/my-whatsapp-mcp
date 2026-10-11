#include "gguf.h"
#include <array>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <string>
#include <unistd.h>

// Synthetic GGUF v3: zero tensors, zero metadata pairs, eight trailing bytes.
// No model, image, network, directory enumeration or environment fallback.
static const std::array<unsigned char, 32> tiny = {'G','G','U','F',3};
static void require(bool ok) { if (!ok) std::abort(); }
static gguf_init_params params() { return {true, nullptr}; }
static void parsed(gguf_context * context) {
    require(context != nullptr);
    require(gguf_get_n_tensors(context) == 0 && gguf_get_n_kv(context) == 0);
    gguf_free(context);
}
static void close_once(int & fd) {
    const int owned = fd; fd = -1;
    require(close(owned) == 0);
}
int main() {
    const char * tmp = std::getenv("TMPDIR");
    require(tmp != nullptr && tmp[0] == '/');
    std::string name = std::string(tmp) + "/gguf-rewind-XXXXXX";
    int fd = mkstemp(name.data());
    require(fd >= 0);
    require(write(fd, tiny.data(), tiny.size()) == static_cast<ssize_t>(tiny.size()));
    require(fsync(fd) == 0);

    // Ordinary filename opens retain their start-at-zero behavior.
    parsed(gguf_init_from_file(name.c_str(), params()));
    parsed(gguf_init_from_file(name.c_str(), params()));

    // Darwin /dev/fd opens duplicate this same open file description. The
    // second parse must work without a caller rewind after the first parse.
    require(lseek(fd, 0, SEEK_SET) == 0);
    const std::string alias = "/dev/fd/" + std::to_string(fd);
    parsed(gguf_init_from_file(alias.c_str(), params()));
    require(lseek(fd, 0, SEEK_CUR) > 0);
    parsed(gguf_init_from_file(alias.c_str(), params()));
    require(fcntl(fd, F_GETFD) >= 0); // wrapper never owns the inherited FD

    // Direct FILE* API still supports GGUF at the supplied nonzero position.
    require(lseek(fd, 0, SEEK_END) == static_cast<off_t>(tiny.size()));
    require(write(fd, tiny.data(), tiny.size()) == static_cast<ssize_t>(tiny.size()));
    FILE * stream = fopen(name.c_str(), "rb");
    require(stream != nullptr);
    require(fseeko(stream, static_cast<off_t>(tiny.size()), SEEK_SET) == 0);
    parsed(gguf_init_from_file_ptr(stream, params()));
    FILE * owned_stream = stream; stream = nullptr;
    require(fclose(owned_stream) == 0);

    // Unseekable filename handles refuse without consuming the supplied FD.
    int pipe_fd[2]; require(pipe(pipe_fd) == 0);
    require(write(pipe_fd[1], tiny.data(), tiny.size()) == static_cast<ssize_t>(tiny.size()));
    close_once(pipe_fd[1]);
    const std::string pipe_alias = "/dev/fd/" + std::to_string(pipe_fd[0]);
    require(gguf_init_from_file(pipe_alias.c_str(), params()) == nullptr);
    require(fcntl(pipe_fd[0], F_GETFD) >= 0);
    close_once(pipe_fd[0]);
    close_once(fd);
    require(unlink(name.c_str()) == 0);
    std::puts("GGUF_FILENAME_REWIND_CASES_4_PASS");
    return 0;
}
