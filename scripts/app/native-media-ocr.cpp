// Private inherited-descriptor OCR adapter for pinned Tesseract 5.5.1.
// argv carries no paths. FD4: LE width/height followed by bounded packed RGB;
// FD5: verified Hebrew traineddata; FD6: verified English traineddata.
// Output is bounded UTF-8 generated text, never a command or file reference.
#include <tesseract/baseapi.h>
#include <algorithm>
#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <memory>
#include <string>
#include <sys/stat.h>
#include <unistd.h>
#include <vector>

static bool read_exact(int fd, char *dst, size_t size, off_t offset) {
    while (size) {
        ssize_t n = pread(fd, dst, size, offset);
        if (n < 0 && errno == EINTR) continue;
        if (n <= 0) return false;
        dst += n; size -= size_t(n); offset += n;
    }
    return true;
}

static bool traineddata(const char *name, std::vector<char> *data) {
    if (!name || !data) return false;
    const char *base = strrchr(name, '/'); base = base ? base + 1 : name;
    int fd = !strcmp(base, "heb.traineddata") ? 5 :
             !strcmp(base, "eng.traineddata") ? 6 : -1;
    if (fd < 0) return false;
    struct stat st{};
    if (fstat(fd, &st) || !S_ISREG(st.st_mode) || st.st_size <= 0 ||
        st.st_size > 32 * 1024 * 1024) return false;
    data->resize(size_t(st.st_size));
    return read_exact(fd, data->data(), data->size(), 0);
}

static uint32_t le32(const unsigned char *p) {
    return uint32_t(p[0]) | uint32_t(p[1]) << 8 | uint32_t(p[2]) << 16 | uint32_t(p[3]) << 24;
}

int main(int argc, char **) {
    if (argc != 1) return 2;
    unsigned char header[8];
    if (!read_exact(4, reinterpret_cast<char *>(header), 8, 0)) return 3;
    uint32_t width = le32(header), height = le32(header + 4);
    if (!width || !height || uint64_t(width) * height > 16000000) return 3;
    const size_t size = size_t(width) * height * 3;
    struct stat st{};
    if (fstat(4, &st) || !S_ISREG(st.st_mode) || st.st_size != off_t(size + 8)) return 3;
    std::vector<unsigned char> rgb(size);
    if (!read_exact(4, reinterpret_cast<char *>(rgb.data()), size, 8)) return 3;
    tesseract::TessBaseAPI api;
    if (api.Init("", 0, "heb+eng", tesseract::OEM_LSTM_ONLY, nullptr, 0,
                 nullptr, nullptr, false, traineddata) != 0) return 4;
    std::vector<std::string> languages;
    api.GetLoadedLanguagesAsVector(&languages);
    if (std::find(languages.begin(), languages.end(), "heb") == languages.end() ||
        std::find(languages.begin(), languages.end(), "eng") == languages.end()) return 4;
    api.SetPageSegMode(tesseract::PSM_AUTO);
    api.SetImage(rgb.data(), int(width), int(height), 3, int(width) * 3);
    api.SetSourceResolution(300);
    if (api.Recognize(nullptr) != 0) return 5;
    std::unique_ptr<char[]> result(api.GetUTF8Text());
    if (!result) return 5;
    const size_t length = strnlen(result.get(), 65537);
    if (length > 65536) return 6;
    if (fwrite(result.get(), 1, length, stdout) != length || fflush(stdout)) return 5;
    api.End();
    return 0;
}
