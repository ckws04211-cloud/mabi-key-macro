package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	_ "image/png"
)

// logo.png: 마비노기 팬사이트 키트의 공식 심볼(symbol_B, 검정 라인 + 알파 채널).
// 앱 안에서는 다크 골드 테마에 맞춰 라인 색만 골드로 바꿔서 쓴다(형태 변형 없음,
// 알파로 표현된 원래 윤곽선은 그대로 유지 — 팬사이트 키트가 허용하는 "배경에
// 맞춰 붙여 쓰기"의 연장선).
//
//go:embed logo.png
var logoPNGBytes []byte

func decodeLogoImage() (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(logoPNGBytes))
	if err != nil {
		return nil, err
	}
	return recolor(img, colPrimary), nil
}

// recolor: 원본 이미지의 알파(형태)는 그대로 두고 RGB만 지정한 색으로 바꾼다.
func recolor(src image.Image, col color.NRGBA) image.Image {
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			_, _, _, a := src.At(x, y).RGBA()
			dst.SetNRGBA(x, y, color.NRGBA{R: col.R, G: col.G, B: col.B, A: uint8(a >> 8)})
		}
	}
	return dst
}
