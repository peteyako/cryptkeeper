package server

import "rsc.io/qr"

func qrEncode(text string) (*qr.Code, error) { return qr.Encode(text, qr.M) }
