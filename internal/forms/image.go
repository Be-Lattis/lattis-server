package forms

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
)

// maxImage : taille d'une image envoyée (avant réduction).
const maxImage = 5 << 20

var errTooBig = errors.New("image trop grosse")

// shrink lit une image PNG ou JPEG, la réduit (logo : 512 px, bannière :
// 1600 × 600 au plus) et la réencode : les métadonnées (EXIF, position…)
// disparaissent.
func shrink(r io.Reader, kind string) ([]byte, string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxImage+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxImage {
		return nil, "", errTooBig
	}
	img, format, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, "", err
	}
	maxW, maxH := 512, 512
	if kind == "banner" {
		maxW, maxH = 1600, 600
	}
	img = fit(img, maxW, maxH)
	var out bytes.Buffer
	if format == "jpeg" {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 85})
		return out.Bytes(), "image/jpeg", err
	}
	err = png.Encode(&out, img)
	return out.Bytes(), "image/png", err
}

// fit réduit l'image dans maxW × maxH (moyenne des pixels couverts).
func fit(src image.Image, maxW, maxH int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxW && h <= maxH {
		return src
	}
	scale := float64(maxW) / float64(w)
	if s := float64(maxH) / float64(h); s < scale {
		scale = s
	}
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := b.Min.Y+y*h/nh, b.Min.Y+(y+1)*h/nh
		for x := 0; x < nw; x++ {
			x0, x1 := b.Min.X+x*w/nw, b.Min.X+(x+1)*w/nw
			var r, g, bl, a, n uint64
			for yy := y0; yy < max(y1, y0+1); yy++ {
				for xx := x0; xx < max(x1, x0+1); xx++ {
					c := color.NRGBAModel.Convert(src.At(xx, yy)).(color.NRGBA)
					r, g, bl, a, n = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A), n+1
				}
			}
			dst.SetNRGBA(x, y, color.NRGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		}
	}
	return dst
}

// ShrinkLogo : comme le logo d'un formulaire (PNG ou JPEG, 512 px au plus,
// métadonnées retirées) ; sert aussi au logo du serveur (administration).
func ShrinkLogo(r io.Reader) ([]byte, string, error) { return shrink(r, "logo") }

// ErrTooBig : image de plus de 5 Mo.
var ErrTooBig = errTooBig
