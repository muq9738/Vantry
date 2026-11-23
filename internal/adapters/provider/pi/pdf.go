package pi

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	maxPDFExtractedBytes = 256 << 10
	maxPDFStreamBytes    = 16 << 20
)

var (
	pdfPageTypePattern = regexp.MustCompile(`/Type\s*/Page\b`)
	pdfBfCharPattern   = regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`)
	pdfBfRangePattern  = regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`)
	pdfHexTokenPattern = regexp.MustCompile(`<([0-9A-Fa-f\s]+)>`)
	pdfTextBlockRegex  = regexp.MustCompile(`(?s)\bBT\b(.*?)\bET\b`)
)

func formatPDFSize(size int64) string {
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

// extractPDFText validates the PDF container signature, counts pages, decodes
// ToUnicode CMap mappings and uncompressed/FlateDecode content streams, and
// extracts readable text runs bounded to maxPDFExtractedBytes.
func extractPDFText(data []byte) (string, int, error) {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	if !bytes.Contains(head, []byte("%PDF-")) {
		return "", 0, errors.New("missing %PDF- header")
	}

	pages := len(pdfPageTypePattern.FindAllIndex(data, -1))
	streams := collectPDFStreams(data)
	cmap := map[string]string{}
	for _, s := range streams {
		if bytes.Contains(s, []byte("begincmap")) || bytes.Contains(s, []byte("beginbfchar")) || bytes.Contains(s, []byte("beginbfrange")) {
			parseToUnicodeCMap(s, cmap)
		}
	}

	var out strings.Builder
	for _, s := range streams {
		if bytes.Contains(s, []byte("begincmap")) {
			continue
		}
		blocks := pdfTextBlockRegex.FindAllSubmatch(s, -1)
		if len(blocks) == 0 {
			continue
		}
		for _, m := range blocks {
			chunk := extractTextBlock(m[1], cmap)
			chunk = normalizePDFLineWhitespace(chunk)
			if chunk == "" {
				continue
			}
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(chunk)
			if out.Len() >= maxPDFExtractedBytes {
				text := truncateUTF8(out.String(), maxPDFExtractedBytes) + "\n…[PDF text truncated]"
				return text, pages, nil
			}
		}
	}
	return strings.TrimSpace(out.String()), pages, nil
}

func collectPDFStreams(data []byte) [][]byte {
	var streams [][]byte
	offset := 0
	for offset < len(data) {
		rel := bytes.Index(data[offset:], []byte("stream"))
		if rel < 0 {
			break
		}
		pos := offset + rel
		// Require "stream" not to be "endstream".
		if pos >= 3 && bytes.Equal(data[pos-3:pos+6], []byte("endstream")) {
			offset = pos + 6
			continue
		}
		dictStart := max(0, pos-512)
		dictSlice := data[dictStart:pos]
		bodyStart := pos + len("stream")
		if bodyStart < len(data) && data[bodyStart] == '\r' {
			bodyStart++
		}
		if bodyStart < len(data) && data[bodyStart] == '\n' {
			bodyStart++
		} else if bodyStart == pos+len("stream") {
			offset = pos + 6
			continue
		}
		endRel := bytes.Index(data[bodyStart:], []byte("endstream"))
		if endRel < 0 {
			break
		}
		rawStream := bytes.TrimRight(data[bodyStart:bodyStart+endRel], "\r\n")
		offset = bodyStart + endRel + len("endstream")

		if bytes.Contains(dictSlice, []byte("/Subtype /Image")) || bytes.Contains(dictSlice, []byte("/Subtype/Image")) {
			continue
		}
		if bytes.Contains(dictSlice, []byte("/FlateDecode")) {
			if decoded, err := inflatePDFStream(rawStream); err == nil && len(decoded) > 0 {
				streams = append(streams, decoded)
				continue
			}
		}
		if len(rawStream) > 0 && len(rawStream) <= maxPDFStreamBytes {
			streams = append(streams, rawStream)
		}
	}
	return streams
}

func inflatePDFStream(raw []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxPDFStreamBytes+1))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	if len(out) > maxPDFStreamBytes {
		return nil, errors.New("pdf stream exceeds limit")
	}
	return out, nil
}

func parseToUnicodeCMap(stream []byte, cmap map[string]string) {
	text := string(stream)
	for _, sec := range pdfBfCharPattern.FindAllStringSubmatch(text, -1) {
		tokens := pdfHexTokenPattern.FindAllStringSubmatch(sec[1], -1)
		for i := 0; i+1 < len(tokens); i += 2 {
			src := cleanHex(tokens[i][1])
			dst := decodeHexUTF16BE(tokens[i+1][1])
			if src != "" && dst != "" {
				cmap[src] = dst
			}
		}
	}
	for _, sec := range pdfBfRangePattern.FindAllStringSubmatch(text, -1) {
		lines := strings.Split(sec[1], "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if bracket := strings.IndexByte(line, '['); bracket >= 0 {
				headTokens := pdfHexTokenPattern.FindAllStringSubmatch(line[:bracket], -1)
				arrTokens := pdfHexTokenPattern.FindAllStringSubmatch(line[bracket:], -1)
				if len(headTokens) >= 2 {
					startCode, ok1 := parseHexUint(headTokens[0][1])
					endCode, ok2 := parseHexUint(headTokens[1][1])
					width := len(cleanHex(headTokens[0][1]))
					if ok1 && ok2 && endCode >= startCode && endCode-startCode <= 1024 {
						for idx := uint32(0); idx <= endCode-startCode && int(idx) < len(arrTokens); idx++ {
							key := fmt.Sprintf("%0*X", width, startCode+idx)
							if val := decodeHexUTF16BE(arrTokens[idx][1]); val != "" {
								cmap[key] = val
							}
						}
					}
				}
				continue
			}
			tokens := pdfHexTokenPattern.FindAllStringSubmatch(line, -1)
			if len(tokens) >= 3 {
				startCode, ok1 := parseHexUint(tokens[0][1])
				endCode, ok2 := parseHexUint(tokens[1][1])
				dstBase := decodeHexUTF16BE(tokens[2][1])
				width := len(cleanHex(tokens[0][1]))
				runes := []rune(dstBase)
				if ok1 && ok2 && endCode >= startCode && endCode-startCode <= 4096 && len(runes) == 1 {
					for idx := uint32(0); idx <= endCode-startCode; idx++ {
						key := fmt.Sprintf("%0*X", width, startCode+idx)
						cmap[key] = string(runes[0] + rune(idx))
					}
				}
			}
		}
	}
}

func cleanHex(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			if c >= 'a' && c <= 'f' {
				c -= 'a' - 'A'
			}
			b.WriteByte(c)
		}
	}
	out := b.String()
	if len(out)%2 == 1 {
		out += "0"
	}
	return out
}

func parseHexUint(s string) (uint32, bool) {
	h := cleanHex(s)
	if h == "" || len(h) > 8 {
		return 0, false
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return 0, false
	}
	var v uint32
	for _, b := range raw {
		v = (v << 8) | uint32(b)
	}
	return v, true
}

func decodeHexUTF16BE(s string) string {
	raw, err := hex.DecodeString(cleanHex(s))
	if err != nil || len(raw) == 0 {
		return ""
	}
	if len(raw) >= 2 && len(raw)%2 == 0 {
		u16 := make([]uint16, len(raw)/2)
		for i := range u16 {
			u16[i] = uint16(raw[2*i])<<8 | uint16(raw[2*i+1])
		}
		return string(utf16.Decode(u16))
	}
	return string(raw)
}

func extractTextBlock(block []byte, cmap map[string]string) string {
	var out strings.Builder
	var pending []string
	inArray := false

	flushPending := func() {
		for _, p := range pending {
			out.WriteString(p)
		}
		pending = pending[:0]
	}

	for i := 0; i < len(block); {
		c := block[i]
		switch {
		case c == '%':
			for i < len(block) && block[i] != '\n' && block[i] != '\r' {
				i++
			}
		case c == '(':
			lit, next := readPDFLiteralString(block, i)
			pending = append(pending, decodePDFStringBytes(lit, cmap))
			i = next
		case c == '<' && (i+1 >= len(block) || block[i+1] != '<'):
			end := bytes.IndexByte(block[i+1:], '>')
			if end < 0 {
				i = len(block)
				continue
			}
			hexStr := string(block[i+1 : i+1+end])
			pending = append(pending, decodePDFHexString(hexStr, cmap))
			i += end + 2
		case c == '[':
			inArray = true
			pending = pending[:0]
			i++
		case c == ']':
			inArray = false
			i++
		case c == '/':
			i++
			for i < len(block) && !isPDFDelimiter(block[i]) && !isPDFSpace(block[i]) {
				i++
			}
			if !inArray {
				pending = pending[:0]
			}
		case isPDFSpace(c):
			i++
		default:
			start := i
			for i < len(block) && !isPDFDelimiter(block[i]) && !isPDFSpace(block[i]) {
				i++
			}
			if i == start {
				i++
				continue
			}
			tok := string(block[start:i])
			switch tok {
			case "Tj", "TJ":
				flushPending()
			case "'", `"`:
				out.WriteByte('\n')
				flushPending()
			case "T*", "Td", "TD":
				if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
					out.WriteByte('\n')
				}
				pending = pending[:0]
			default:
				if inArray {
					// Large negative kerning in TJ arrays represents a word space.
					if tok != "" && tok[0] == '-' && len(tok) >= 4 {
						pending = append(pending, " ")
					}
				} else {
					pending = pending[:0]
				}
			}
		}
	}
	return out.String()
}

func isPDFSpace(b byte) bool {
	return b == 0 || b == '\t' || b == '\n' || b == '\f' || b == '\r' || b == ' '
}

func isPDFDelimiter(b byte) bool {
	switch b {
	case '(', ')', '<', '>', '[', ']', '{', '}', '/', '%':
		return true
	}
	return false
}

func readPDFLiteralString(src []byte, start int) ([]byte, int) {
	var out []byte
	depth := 0
	i := start
	for i < len(src) {
		b := src[i]
		if i == start && b == '(' {
			depth = 1
			i++
			continue
		}
		if b == '\\' && i+1 < len(src) {
			i++
			esc := src[i]
			switch esc {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '(', ')', '\\':
				out = append(out, esc)
			case '\r':
				if i+1 < len(src) && src[i+1] == '\n' {
					i++
				}
			case '\n':
			default:
				if esc >= '0' && esc <= '7' {
					val := byte(esc - '0')
					for k := 0; k < 2 && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '7'; k++ {
						i++
						val = val*8 + (src[i] - '0')
					}
					out = append(out, val)
				} else {
					out = append(out, esc)
				}
			}
			i++
			continue
		}
		if b == '(' {
			depth++
			out = append(out, b)
			i++
			continue
		}
		if b == ')' {
			depth--
			i++
			if depth <= 0 {
				break
			}
			out = append(out, b)
			continue
		}
		out = append(out, b)
		i++
	}
	return out, i
}

func decodePDFHexString(hexStr string, cmap map[string]string) string {
	clean := cleanHex(hexStr)
	if clean == "" {
		return ""
	}
	if len(cmap) > 0 {
		var mapped strings.Builder
		matched := false
		for i := 0; i < len(clean); {
			if i+4 <= len(clean) {
				if s, ok := cmap[clean[i:i+4]]; ok {
					mapped.WriteString(s)
					matched = true
					i += 4
					continue
				}
			}
			if i+2 <= len(clean) {
				if s, ok := cmap[clean[i:i+2]]; ok {
					mapped.WriteString(s)
					matched = true
					i += 2
					continue
				}
			}
			i += 2
		}
		if matched {
			return mapped.String()
		}
	}
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return ""
	}
	return decodePDFStringBytes(raw, nil)
}

func decodePDFStringBytes(raw []byte, cmap map[string]string) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		u16 := make([]uint16, (len(raw)-2)/2)
		for i := range u16 {
			u16[i] = uint16(raw[2+2*i])<<8 | uint16(raw[2+2*i+1])
		}
		return string(utf16.Decode(u16))
	}
	if len(cmap) > 0 {
		h := strings.ToUpper(hex.EncodeToString(raw))
		if len(h)%4 == 0 {
			var b strings.Builder
			all := true
			for i := 0; i < len(h); i += 4 {
				if s, ok := cmap[h[i:i+4]]; ok {
					b.WriteString(s)
				} else {
					all = false
					break
				}
			}
			if all && b.Len() > 0 {
				return b.String()
			}
		}
	}
	if utf8.Valid(raw) {
		return string(raw)
	}
	var b strings.Builder
	for _, ch := range raw {
		if ch >= 0x20 || ch == '\n' || ch == '\t' {
			b.WriteRune(rune(ch))
		}
	}
	return b.String()
}

func normalizePDFLineWhitespace(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range lines {
		var b strings.Builder
		prevSpace := false
		for _, r := range line {
			if r == 0 || (unicode.IsControl(r) && r != '\t') {
				continue
			}
			if unicode.IsSpace(r) {
				if !prevSpace {
					b.WriteByte(' ')
					prevSpace = true
				}
				continue
			}
			prevSpace = false
			b.WriteRune(r)
		}
		cleaned := strings.TrimSpace(b.String())
		if cleaned != "" {
			out = append(out, cleaned)
		}
	}
	return strings.Join(out, "\n")
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
