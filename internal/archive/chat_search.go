package archive

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// chatNameKey lets a name match with or without spaces, punctuation, case, or
// Hebrew marks. It does not translate or change letters in the saved name.
func chatNameKey(name string) string {
	var key strings.Builder
	key.Grow(len(name))
	for _, r := range NormalizeLexical(name) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			key.WriteRune(r)
		}
	}
	return key.String()
}

func chatListFilter(request ChatListRequest) (queryKey, chatType string, err error) {
	if len(request.Query) > 256 || !utf8.ValidString(request.Query) || strings.ContainsRune(request.Query, 0) {
		return "", "", ErrInvalidQuery
	}
	queryKey = chatNameKey(request.Query)
	if request.Query != "" && queryKey == "" {
		return "", "", ErrInvalidQuery
	}
	chatType = request.ChatType
	if chatType == "" {
		chatType = "all"
	}
	switch chatType {
	case "all", "direct", "group":
		return queryKey, chatType, nil
	default:
		return "", "", ErrInvalidQuery
	}
}
