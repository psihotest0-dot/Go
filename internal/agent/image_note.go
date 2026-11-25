package agent

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The model says it sees pictures. In the owner's Telegram conversation of 2026-11-24 (21:19) it
// answered "как виждаш снимката от камерата" with "поредица от числови матрици, които представляват
// пиксели… многоизмерни вектори (embeddings)". It sees none: the local model reads text only, and a
// photo or a scan the person sends is read by OCR (Tesseract) on the server - the text of one that is
// no bill is attached to the conversation (bills.Service.WithAttachments). A message about how the
// assistant sees or reads pictures gets a note for the model on that one request; it is not stored
// and costs the other turns nothing (a line of the system prompt would be read every turn).

const imageNote = "[Note for the assistant, not written by the user: you do not see pictures. A photo or a scan the user sends " +
	"is read by OCR (Tesseract) on the server, and you get only the text it recognised - as an attached file \"<name>.txt\" " +
	"when it is not a bill - never the picture itself: no pixels, colours, faces, objects or layout reach you, and the model " +
	"you run on reads text only. If it matters to the answer, say so plainly in the user's language.]"

// imageNoteMaxRunes: a longer message is about something else, with a picture in passing.
const imageNoteMaxRunes = 400

var (
	// imageWords: a picture, a photo, a scan, the camera, OCR or vision.
	imageWords = regexp.MustCompile(`(?i)(?:снимк|снима|изображени|картин|фото|камера|скан|vision|ocr|image|photo|picture|camera)`)
	// imageAbout: the message asks how the assistant sees, reads or knows them.
	imageAbout = regexp.MustCompile(`(?i)(?:виждаш|видиш|разпознава|разпознаеш|възприема|четеш|прочет|разбираш|обработва|как\s|можеш|нужно|трябва\s+ли|какво\s+(?:пише|има)|see|read|recogni[sz]e|how\s)`)
)

// asksAboutImages tells whether a message asks how the assistant sees or reads pictures.
func asksAboutImages(message string) bool {
	m := strings.TrimSpace(message)
	if m == "" || utf8.RuneCountInString(m) > imageNoteMaxRunes {
		return false
	}
	return imageWords.MatchString(m) && imageAbout.MatchString(m)
}
