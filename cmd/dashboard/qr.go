package main

import (
	"encoding/base64"

	"rsc.io/qr"
)

// qrDataURL renders text as a QR PNG and returns a data: URL ready to embed
// as an <img src=...>. Quality 'M' (~15% recoverable) is fine for screen scanning.
func qrDataURL(text string) string {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG())
}
