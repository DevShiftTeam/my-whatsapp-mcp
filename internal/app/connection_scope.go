package app

import "github.com/Bodokh/my-whatsapp-mcp/internal/archive"

// Grant publication checks persisted effective authority against explicit input.
func nativeGrantWithinPolicy(scope archive.ReadScope, policy archive.CredentialPolicy) bool {
	if scope.Account != policy.Account {
		return false
	}
	if !policy.AllChats {
		if scope.AllChats {
			return false
		}
		allowed := make(map[archive.ChatID]bool, len(policy.Chats))
		for _, chat := range policy.Chats {
			allowed[chat] = true
		}
		for _, chat := range scope.Chats {
			if !allowed[chat] {
				return false
			}
		}
	}
	if !policy.From.IsZero() && (scope.From.IsZero() || scope.From.Before(policy.From)) {
		return false
	}
	if !policy.Until.IsZero() && (scope.Until.IsZero() || scope.Until.After(policy.Until)) {
		return false
	}
	if scope.Rolling < 0 {
		return false
	}
	return policy.Rolling <= 0 || scope.Rolling > 0 && scope.Rolling <= policy.Rolling
}
