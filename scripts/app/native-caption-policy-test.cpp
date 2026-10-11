// Model-free check of the patched production formatter. No GGUF/vision proof.
#include "chat.h"

#include <iostream>
#include <map>
#include <stdexcept>
#include <string>

static void require(bool condition, const char * message) {
    if (!condition) {
        throw std::runtime_error(message);
    }
}

static void check(const common_chat_templates * tmpls, bool thinking,
                  std::map<std::string, std::string> kwargs, bool expect_thinking) {
    kwargs["fixture_probe"] = "\"kwargs-received\"";
    common_chat_msg user;
    user.role = "user";
    user.content = "<__media__>Hebrew fixture";
    const std::string rendered = common_chat_format_single(tmpls, {}, user, true, true, thinking, kwargs);
    require(rendered.find("kwargs-received") != std::string::npos, "template kwargs were dropped");
    require(rendered.find("<|im_start|>user\n<__media__>Hebrew fixture<|im_end|>\n") != std::string::npos,
            "user ChatML or content changed");
    const auto marker = rendered.find("<__media__>");
    require(marker != std::string::npos && rendered.find("<__media__>", marker + 1) == std::string::npos,
            "media marker missing or duplicated");
    const std::string suffix = expect_thinking ? "<|im_start|>assistant\n<think>\n" :
                                               "<|im_start|>assistant\n<think>\n\n</think>\n\n";
    require(rendered.size() >= suffix.size() && rendered.compare(rendered.size() - suffix.size(), suffix.size(), suffix) == 0,
            "thinking generation prefix did not follow the actual formatter inputs");
    require((rendered.find("</think>") != std::string::npos) != expect_thinking,
            "thinking branch mismatch");
}

int main() {
    try {
        // Guard the probe: initialization renders capability examples without kwargs.
        const std::string source =
            "{% if fixture_probe is defined %}{{ fixture_probe }}{% endif %}"
            "{% for message in messages %}{{ '<|im_start|>' + message['role'] + '\n' + message['content'] + '<|im_end|>\n' }}{% endfor %}"
            "{% if add_generation_prompt %}{{ '<|im_start|>assistant\n' }}"
            "{% if enable_thinking %}{{ '<think>\n' }}{% else %}{{ '<think>\n\n</think>\n\n' }}{% endif %}{% endif %}";
        auto tmpls = common_chat_templates_init(nullptr, source, "", "");
        require(bool(tmpls), "model-free template initialization failed");
        check(tmpls.get(), false, {}, false); // bool reaches actual template inputs
        check(tmpls.get(), true, {{"enable_thinking", "false"}}, false); // kwargs override reaches inputs
        check(tmpls.get(), true, {}, true); // positive thinking branch; no false override
        check(tmpls.get(), false, {{"enable_thinking", "false"}}, false); // production combination
        std::cout << "PASS caption-v2 actual formatter policy (model-free)\n";
        return 0;
    } catch (const std::exception & error) {
        std::cerr << "FAIL caption-v2 formatter policy: " << error.what() << '\n';
        return 1;
    }
}
