package betty

import "strings"

// restInChat is what she says when her spoken answers would queue too long
// and the rest went to chat (hivecommons/hive#18590), by primary language
// subtag; English is the fallback.
var restInChat = map[string]string{
	"en": "I've put the rest in chat.",
	"es": "El resto lo dejé en el chat.",
	"fr": "J'ai mis la suite dans le chat.",
	"de": "Den Rest habe ich in den Chat geschrieben.",
	"pt": "Coloquei o resto no chat.",
	"it": "Il resto l'ho scritto in chat.",
}

// RestInChatText is the spoken note for answers moved to chat, in the
// persona's language.
func (e *Engine) RestInChatText() string {
	if t, ok := restInChat[primaryTag(e.s.Persona.Language)]; ok {
		return t
	}
	return restInChat["en"]
}

// primaryTag is a BCP-47 tag's primary language subtag ("en-GB" is "en").
func primaryTag(lang string) string {
	return strings.ToLower(strings.SplitN(strings.TrimSpace(lang), "-", 2)[0])
}
