package domains

import (
	_ "embed"
	"net/http"
)

type Photo struct {
	width  uint
	height uint
	data   []byte
	mime   string
	id     string
}

//go:embed static/default.jpg
var fallbackImage []byte

var fallbackPhoto = Photo{
	width:  160,
	height: 160,
	data:   fallbackImage,
	mime:   "image/jpeg",
	id:     "fallback",
}

func GetDefaultPhoto() *Photo {
	return &fallbackPhoto
}

func NewPhoto(w uint, h uint, d []byte, id string) *Photo {
	return &Photo{
		width:  w,
		height: h,
		data:   d,
		id:     id,
		mime:   http.DetectContentType(d),
	}
}

func (p *Photo) Data() []byte {
	return p.data
}

func (p *Photo) MIME() string {
	return p.mime
}

func (p *Photo) Dimensions() (uint, uint) {
	return p.width, p.height
}

func (p *Photo) ID() string {
	return p.id
}
