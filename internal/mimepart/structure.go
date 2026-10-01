package mimepart

import "strings"

// Flatten derives textBody, htmlBody and attachments from the part tree,
// with the algorithm RFC 8621 4.1.4 gives. It is ported as written,
// including the one thing that is easy to get wrong in a port: the
// reference algorithm stops collecting into a list by setting its own
// variable to null, which affects the rest of that call and the calls it
// makes, but not its caller. A nil pointer here is that null.
func Flatten(root *Part) (text, html, attached []*Part) {
	var t, h, a []*Part

	walkStructure([]*Part{root}, "mixed", false, &h, &t, &a)

	return t, h, a
}

// HasAttachment is what RFC 8621 4.1.4 suggests: an attachment that is
// not marked inline is one a client should offer for download.
func HasAttachment(attached []*Part) bool {
	for _, p := range attached {
		if Disposition(p) != DispInline {
			return true
		}
	}

	return false
}

// MessageHasAttachment is HasAttachment of a whole message, taken apart
// as JMAP takes it apart: a message beyond the limits of the walk is one
// opaque part, and with that an attachment.
func MessageHasAttachment(raw []byte) bool {
	_, _, attached := Flatten(ParseOrOpaque(raw))

	return HasAttachment(attached)
}

// walkStructure is parseStructure of RFC 8621 4.1.4.
func walkStructure(parts []*Part, multipartType string, inAlternative bool, htmlBody, textBody, attachments *[]*Part) {
	textLength, htmlLength := sliceLen(textBody), sliceLen(htmlBody)

	for i, part := range parts {
		switch {
		case strings.HasPrefix(part.MediaType, multipartPrefix):
			sub := strings.TrimPrefix(part.MediaType, multipartPrefix)
			walkStructure(part.Children, sub, inAlternative || sub == subAlternative, htmlBody, textBody, attachments)
		case !isInlinePart(part, i, multipartType):
			push(attachments, part)
		case multipartType == subAlternative:
			placeAlternative(part, htmlBody, textBody, attachments)
		default:
			htmlBody, textBody = placeInline(part, inAlternative, htmlBody, textBody, attachments)
		}
	}

	if multipartType == subAlternative && textBody != nil && htmlBody != nil {
		balanceAlternative(textBody, htmlBody, textLength, htmlLength)
	}
}

// placeInline puts an inline part outside a multipart/alternative into
// the lists, and returns them as the rest of the walk is to see them:
// inside an alternative a plain part ends the collection of HTML and an
// HTML part ends the collection of plain text, for this call and the
// ones after it, as the reference algorithm's nulling does.
func placeInline(part *Part, inAlternative bool, htmlBody, textBody, attachments *[]*Part) (html, text *[]*Part) {
	if inAlternative {
		if part.MediaType == MediaTextPlain {
			htmlBody = nil
		}

		if part.MediaType == MediaTextHTML {
			textBody = nil
		}
	}

	push(textBody, part)
	push(htmlBody, part)

	if (textBody == nil || htmlBody == nil) && isInlineMedia(part.MediaType) {
		push(attachments, part)
	}

	return htmlBody, textBody
}

// isInlinePart is the isInline test of the reference algorithm: a body
// part rather than an attachment.
func isInlinePart(part *Part, i int, multipartType string) bool {
	if Disposition(part) == DispAttachment {
		return false
	}

	if part.MediaType != MediaTextPlain && part.MediaType != MediaTextHTML && !isInlineMedia(part.MediaType) {
		return false
	}

	// In multipart/related only the first part can be inline; a text part
	// with a file name that is not first is taken for an attachment.
	return i == 0 || (multipartType != "related" && (isInlineMedia(part.MediaType) || FileName(part) == ""))
}

// placeAlternative puts one inline part of a multipart/alternative into
// the list of its kind.
func placeAlternative(part *Part, htmlBody, textBody, attachments *[]*Part) {
	switch part.MediaType {
	case MediaTextPlain:
		push(textBody, part)
	case MediaTextHTML:
		push(htmlBody, part)
	default:
		push(attachments, part)
	}
}

// balanceAlternative fills in the kind an alternative lacks: an
// alternative with only an HTML part gives it to textBody as well, and
// one with only a text part gives it to htmlBody.
func balanceAlternative(textBody, htmlBody *[]*Part, textLength, htmlLength int) {
	if textLength == len(*textBody) && htmlLength != len(*htmlBody) {
		*textBody = append(*textBody, (*htmlBody)[htmlLength:]...)
	}

	if htmlLength == len(*htmlBody) && textLength != len(*textBody) {
		*htmlBody = append(*htmlBody, (*textBody)[textLength:]...)
	}
}

// push appends to a list that is still being collected.
func push(list *[]*Part, part *Part) {
	if list != nil {
		*list = append(*list, part)
	}
}

// sliceLen is the length of a list, or -1 for one that is not collected.
func sliceLen(list *[]*Part) int {
	if list == nil {
		return -1
	}

	return len(*list)
}

// isInlineMedia is the isInlineMediaType of the reference algorithm.
func isInlineMedia(mt string) bool {
	return strings.HasPrefix(mt, "image/") || strings.HasPrefix(mt, "audio/") || strings.HasPrefix(mt, "video/")
}
