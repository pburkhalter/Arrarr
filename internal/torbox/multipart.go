package torbox

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
)

// buildNZBMultipart writes the file part with a hand-rolled MIME header
// because TorBox rejects "application/x-nzb;charset=utf-8" — the charset
// parameter that mime/multipart's CreateFormFile would attach.
func buildNZBMultipart(filename string, body []byte, password string) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	hdr := textproto.MIMEHeader{}
	hdr.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename=%q`, sanitizeFilename(filename)))
	hdr.Set("Content-Type", "application/x-nzb")
	part, err := w.CreatePart(hdr)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(body); err != nil {
		return nil, "", err
	}
	if password != "" {
		if err := w.WriteField("password", password); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &buf, w.FormDataContentType(), nil
}

func sanitizeFilename(s string) string {
	if s == "" {
		return "upload.nzb"
	}
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '"' || r == '\\' || r == '\n' || r == '\r' {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "upload.nzb"
	}
	return string(out)
}
