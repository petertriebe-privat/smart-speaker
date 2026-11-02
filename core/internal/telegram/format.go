package telegram

import (
	"strings"

	"echo-assistant/internal/chatcore"
	"echo-assistant/internal/textutil"
)

// escape: text for Telegram's HTML mode.
func escape(text string) string { return textutil.HTML(text) }

// plain turns Telegram HTML into plain text, for when Telegram rejects the markup ("can't parse
// entities"). Links keep their address: <a href="u">Text</a> -> "Text (u)"; without withLinks
// (read out): "Text".
func plain(html string, withLinks bool) string {
	var text strings.Builder
	link := ""
	for i := 0; i < len(html); {
		if html[i] != '<' {
			text.WriteByte(html[i])
			i++
			continue
		}
		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			text.WriteString(html[i:])
			break
		}
		end += i
		tag := html[i : end+1]
		href := strings.Index(tag, `href="`)
		if strings.HasPrefix(tag, "<a ") && href > 0 {
			rest := tag[href+6:]
			if q := strings.IndexByte(rest, '"'); q >= 0 {
				link = rest[:q]
			}
		} else if tag == "</a>" && link != "" {
			if withLinks {
				text.WriteString(" (" + link + ")")
			}
			link = ""
		}
		i = end + 1
	}
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&quot;", `"`, "&amp;", "&").Replace(text.String())
}

// linksToHTML turns Markdown links as the web search adds its sources
// ("([timeanddate.de](https://...))") into Telegram HTML; Telegram would show them raw.
func linksToHTML(text string) string {
	return chatcore.MarkdownLinks(text, func(label, url string) string {
		url = strings.ReplaceAll(url, "&amp;", "&")
		url = strings.ReplaceAll(url, "&", "&amp;")
		url = strings.ReplaceAll(url, `"`, "%22")
		return `<a href="` + url + `">` + label + `</a>`
	})
}

// spoken: the answer read out for a voice reply, without markup and links, cut when long (the
// text comes as well). "" if it has nothing to say.
func spoken(answer string, maxChars int) string {
	text := chatcore.StripSources(answer)
	text = plain(linksToHTML(text), false)
	text = chatcore.StripAddresses(text)
	text = strings.ReplaceAll(text, "• ", "")
	return chatcore.Spoken(text, maxChars)
}
