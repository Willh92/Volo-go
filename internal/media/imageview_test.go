package media_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"

	"github.com/Willh92/Volo-go/internal/media"
)

// synthesizeMinimalAVIF builds a valid ISOBMFF AVIF container with `ftyp` (`avif`),
// `meta` FullBox -> `pitm` + `iprp` -> `ipco` (`ispe` width x height + optional `irot`) + `ipma`.
func synthesizeMinimalAVIF(w, h uint32, irotQuarterTurns byte) []byte {
	box := func(typ string, payload []byte) []byte {
		var b bytes.Buffer
		_ = binary.Write(&b, binary.BigEndian, uint32(8+len(payload)))
		b.WriteString(typ)
		b.Write(payload)
		return b.Bytes()
	}

	// ftyp: major="avif", minor=0, compatible=["avif", "mif1"]
	var ftypPay bytes.Buffer
	ftypPay.WriteString("avif")
	_ = binary.Write(&ftypPay, binary.BigEndian, uint32(0))
	ftypPay.WriteString("avifmif1")
	ftyp := box("ftyp", ftypPay.Bytes())

	// pitm (version 0, flags 0, item_ID = 1)
	pitm := box("pitm", []byte{0, 0, 0, 0, 0, 1})

	// ispe (FullBox version 0, flags 0 + width + height)
	var ispePay bytes.Buffer
	ispePay.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(&ispePay, binary.BigEndian, w)
	_ = binary.Write(&ispePay, binary.BigEndian, h)
	ispe := box("ispe", ispePay.Bytes())

	var ipcoChildren bytes.Buffer
	ipcoChildren.Write(ispe)
	assocCount := byte(1)
	if irotQuarterTurns > 0 {
		irot := box("irot", []byte{irotQuarterTurns & 0x03})
		ipcoChildren.Write(irot)
		assocCount = 2
	}
	ipco := box("ipco", ipcoChildren.Bytes())

	// ipma (version 0, flags 0, entry_count=1, item_ID=1, assoc_count, prop indices 1, [2])
	var ipmaPay bytes.Buffer
	ipmaPay.Write([]byte{0, 0, 0, 0})
	_ = binary.Write(&ipmaPay, binary.BigEndian, uint32(1))
	_ = binary.Write(&ipmaPay, binary.BigEndian, uint16(1))
	ipmaPay.WriteByte(assocCount)
	ipmaPay.WriteByte(0x80 | 1)
	if assocCount == 2 {
		ipmaPay.WriteByte(0x80 | 2)
	}
	ipma := box("ipma", ipmaPay.Bytes())
	iprp := box("iprp", append(ipco, ipma...))

	var metaPay bytes.Buffer
	metaPay.Write([]byte{0, 0, 0, 0}) // FullBox version+flags
	metaPay.Write(pitm)
	metaPay.Write(iprp)
	meta := box("meta", metaPay.Bytes())

	mdat := box("mdat", []byte{0x01, 0x02, 0x03, 0x04})

	var out bytes.Buffer
	out.Write(ftyp)
	out.Write(meta)
	out.Write(mdat)
	return out.Bytes()
}

// buildRichMetadataJPEG synthesizes a colorful `w x h` JPEG containing EXIF (with Orientation),
// an ICC color profile (`APP2 ICC_PROFILE`), and an XMP packet (`APP1 http://ns.adobe.com/xap/1.0/\x00`).
func buildRichMetadataJPEG(t *testing.T, w, h int, orientation uint16) ([]byte, []byte, []byte) {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x * 255) / max(1, w-1)),
				G: uint8((y * 255) / max(1, h-1)),
				B: uint8(((x + y) * 127) / max(1, w+h-2)),
				A: 255,
			})
		}
	}
	// Mark top-left quadrant pure bright red so orientation=6 rotation is unmistakable
	for y := 0; y < h/4; y++ {
		for x := 0; x < w/4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 250, G: 10, B: 10, A: 255})
		}
	}

	var baseJPG bytes.Buffer
	if err := jpeg.Encode(&baseJPG, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	jpgBytes := baseJPG.Bytes()

	// 1. EXIF TIFF with Orientation
	var tiff bytes.Buffer
	tiff.WriteString("II\x2a\x00")
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(8))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(1))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(3))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(1))
	_ = binary.Write(&tiff, binary.LittleEndian, orientation)
	_ = binary.Write(&tiff, binary.LittleEndian, uint16(0))
	_ = binary.Write(&tiff, binary.LittleEndian, uint32(0))

	// 2. Synthetic ICC Profile payload
	iccProfile := []byte("MOCK_SRGB_ICC_PROFILE_V4_DATA_PAYLOAD_0123456789ABCDEF")
	// 3. Synthetic XMP XML payload
	xmpPacket := []byte(`<x:xmpmeta xmlns:x="adobe:ns:meta/"><rdf:RDF>VoloXMPTest</rdf:RDF></x:xmpmeta>`)

	writeSeg := func(out *bytes.Buffer, marker byte, payload []byte) {
		out.Write([]byte{0xFF, marker})
		var sz [2]byte
		binary.BigEndian.PutUint16(sz[:], uint16(2+len(payload)))
		out.Write(sz[:])
		out.Write(payload)
	}

	var out bytes.Buffer
	out.Write(jpgBytes[:2]) // SOI
	writeSeg(&out, 0xE1, append([]byte("Exif\x00\x00"), tiff.Bytes()...))
	iccSeg := append([]byte("ICC_PROFILE\x00\x01\x01"), iccProfile...)
	writeSeg(&out, 0xE2, iccSeg)
	xmpSeg := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), xmpPacket...)
	writeSeg(&out, 0xE1, xmpSeg)
	out.Write(jpgBytes[2:])

	return out.Bytes(), iccProfile, xmpPacket
}

func decodeAnyRenderedImage(t *testing.T, ct string, data []byte) image.Image {
	t.Helper()
	if ct == "image/webp" {
		img, err := nativewebp.Decode(bytes.NewReader(data))
		if err != nil {
			img, err = nativewebp.DecodeIgnoreAlphaFlag(bytes.NewReader(data))
		}
		if err != nil {
			t.Fatalf("nativewebp.Decode failed: %v", err)
		}
		return img
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("image.Decode(%s) failed: %v", ct, err)
	}
	return img
}

func TestParseImageViewSpec_DualSyntaxCanonicalKeyAndValidation(t *testing.T) {
	// 1. Non-ImageView queries return hasTransform=false, err=nil
	for _, q := range []string{"", "?", "?v=123", "?download=1&utm_source=rss"} {
		_, has, err := media.ParseImageViewSpec(q)
		if err != nil || has {
			t.Fatalf("ParseImageViewSpec(%q) = (has=%v, err=%v), want (false, nil)", q, has, err)
		}
	}

	// 2. Qiniu path style vs KV style parity (including valueless /strip and &strip)
	qPath := "?imageView2/1/w/960/h/540/format/jpeg/q/82/interlace/1/colors/128/strip/meta-keep-list/xmp,exif/ignore-error/1"
	qKV := "?mode=1&w=960&h=540&format=jpg&q=82&interlace=1&colors=128&strip&meta-keep-list=exif,xmp&ignore-error=1"

	sPath, hasP, errP := media.ParseImageViewSpec(qPath)
	if errP != nil || !hasP {
		t.Fatalf("ParseImageViewSpec(path) failed: has=%v err=%v", hasP, errP)
	}
	sKV, hasK, errK := media.ParseImageViewSpec(qKV)
	if errK != nil || !hasK {
		t.Fatalf("ParseImageViewSpec(kv) failed: has=%v err=%v", hasK, errK)
	}
	if !reflect.DeepEqual(sPath, sKV) {
		t.Fatalf("path spec %+v != KV spec %+v", sPath, sKV)
	}
	if sPath.CanonicalKey() != sKV.CanonicalKey() {
		t.Fatalf("CanonicalKey mismatch: %q vs %q", sPath.CanonicalKey(), sKV.CanonicalKey())
	}
	if !reflect.DeepEqual(sPath.MetaKeepList, []string{"exif", "xmp"}) {
		t.Fatalf("expected sorted MetaKeepList [exif xmp], got %v", sPath.MetaKeepList)
	}

	// 3. Hybrid query (`imageView2/2/w/5000/strip/1&q=75&ignore-error=1`) + >4096 dimension clamping
	sHyb, hasH, errH := media.ParseImageViewSpec("?imageView2/2/w/5000/strip/1&q=75&ignore-error=1")
	if errH != nil || !hasH {
		t.Fatalf("hybrid parse failed: %v", errH)
	}
	if sHyb.Mode != 2 || sHyb.W != 4096 || !sHyb.Strip || sHyb.Quality != 75 || !sHyb.IgnoreError {
		t.Fatalf("unexpected hybrid spec: %+v", sHyb)
	}

	// 4. Pre-extraction of IgnoreError BEFORE validation errors!
	for _, badWithIgnore := range []string{
		"?mode=99&w=400&ignore-error=1",
		"?imageView2/9/w/400/ignore-error/1",
		"?imageView2/1/w/400/q/999/ignore-error/1",
		"?w=0&ignore-error=1",
	} {
		sp, has, err := media.ParseImageViewSpec(badWithIgnore)
		if err == nil || !has {
			t.Fatalf("expected error and hasTransform=true for %q, got has=%v err=%v", badWithIgnore, has, err)
		}
		if !sp.IgnoreError {
			t.Fatalf("expected spec.IgnoreError=true even on validation error for %q", badWithIgnore)
		}
	}

	// 5. Bounds rejection without ignore-error
	for _, bad := range []string{
		"?mode=6&w=100",
		"?w=-5",
		"?h=0",
		"?format=svg",
		"?interlace=2",
		"?q=0",
		"?q=101",
		"?colors=1",
		"?colors=257",
		"?meta-keep-list=gps",
		"?imageView2",
	} {
		if _, _, err := media.ParseImageViewSpec(bad); err == nil {
			t.Fatalf("expected validation error for %q", bad)
		}
	}
}

func TestImageViewEngine_AllSixModesAndPredictDimsParity(t *testing.T) {
	ctx := context.Background()
	eng := media.NewImageViewEngine(8 << 20)

	// Test both landscape (240x160) and portrait (160x240) sources across modes 0..5 + no-upscale
	for _, srcDims := range [][2]int{{240, 160}, {160, 240}} {
		srcW, srcH := srcDims[0], srcDims[1]
		rawJPG, _, _ := buildRichMetadataJPEG(t, srcW, srcH, 1)
		src := media.ImageViewSource{
			Key:         fmt.Sprintf("modes-%dx%d.jpg", srcW, srcH),
			ModTimeNano: 100,
			Size:        int64(len(rawJPG)),
			Ext:         ".jpg",
			Load: func(_ context.Context) ([]byte, error) {
				return rawJPG, nil
			},
		}

		queries := []string{
			"?imageView2/0/w/120/h/90",
			"?imageView2/0/w/120",
			"?imageView2/0/h/80",
			"?imageView2/1/w/100/h/100",
			"?imageView2/1/w/90/h/60",
			"?imageView2/2/w/100/h/100",
			"?imageView2/2/w/120",
			"?imageView2/3/w/100/h/100",
			"?imageView2/4/w/120/h/90",
			"?imageView2/5/w/120/h/90",
			// No-upscale checks (requested box larger than source):
			"?imageView2/2/w/800/h/800",
			"?imageView2/1/w/800/h/100",
			"?imageView2/5/w/800/h/100",
		}

		for _, q := range queries {
			predW, predH := media.PredictTransformedDims(srcW, srcH, q)
			spec, has, err := media.ParseImageViewSpec(q)
			if err != nil || !has {
				t.Fatalf("ParseImageViewSpec(%q): %v", q, err)
			}
			res, err := eng.Transform(ctx, src, spec)
			if err != nil {
				t.Fatalf("Transform(%dx%d, %q) failed: %v", srcW, srcH, q, err)
			}
			img := decodeAnyRenderedImage(t, res.ContentType, res.Data)
			gotW, gotH := img.Bounds().Dx(), img.Bounds().Dy()
			if gotW != predW || gotH != predH {
				t.Fatalf("PredictTransformedDims(%dx%d, %q) = %dx%d, actual rendered = %dx%d",
					srcW, srcH, q, predW, predH, gotW, gotH)
			}
			if gotW > srcW || gotH > srcH {
				t.Fatalf("unexpected upscale on %dx%d with %q -> %dx%d", srcW, srcH, q, gotW, gotH)
			}
		}
	}
}

func TestImageViewEngine_InterlaceAdam7ColorsQualityFormatsAndAVIF(t *testing.T) {
	ctx := context.Background()
	eng := media.NewImageViewEngine(8 << 20)

	// Build a translucent RGBA PNG with 500+ gradient colors + alpha hole
	origNRGBA := image.NewNRGBA(image.Rect(0, 0, 97, 63))
	for y := 0; y < 63; y++ {
		for x := 0; x < 97; x++ {
			a := uint8(255)
			if x < 12 && y < 12 {
				a = 0
			}
			origNRGBA.SetNRGBA(x, y, color.NRGBA{
				R: uint8((x * 13) & 0xFF),
				G: uint8((y * 19) & 0xFF),
				B: uint8(((x + y) * 7) & 0xFF),
				A: a,
			})
		}
	}
	var rawPNGBuf bytes.Buffer
	if err := png.Encode(&rawPNGBuf, origNRGBA); err != nil {
		t.Fatal(err)
	}
	rawPNG := rawPNGBuf.Bytes()
	origDiskPath := filepath.Join(t.TempDir(), "source.png")
	if err := os.WriteFile(origDiskPath, rawPNG, 0o640); err != nil {
		t.Fatal(err)
	}
	origSHA := sha256.Sum256(rawPNG)

	srcPNG := media.ImageViewSource{
		Key:         "2026/11/source.png",
		ModTimeNano: 12345,
		Size:        int64(len(rawPNG)),
		Ext:         ".png",
		Load: func(_ context.Context) ([]byte, error) {
			return os.ReadFile(origDiskPath)
		},
	}

	// 1. Adam7 Interlaced Truecolor+Alpha PNG (`interlace=1`, exact pixel round-trip!)
	specInterlace, _, _ := media.ParseImageViewSpec("?format=png&interlace=1")
	resInterlace, err := eng.Transform(ctx, srcPNG, specInterlace)
	if err != nil {
		t.Fatalf("Adam7 PNG transform failed: %v", err)
	}
	if resInterlace.ContentType != "image/png" || len(resInterlace.Data) < 33 {
		t.Fatalf("unexpected PNG output")
	}
	// PNG signature (8) + IHDR len (4) + "IHDR" (4) + width(4)+height(4)+bitDepth(1)+colorType(1)+comp(1)+filter(1)+interlace(1) -> index 28
	if resInterlace.Data[28] != 1 {
		t.Fatalf("expected PNG IHDR interlace method byte = 1 (Adam7), got %d", resInterlace.Data[28])
	}
	decInterlaced, err := png.Decode(bytes.NewReader(resInterlace.Data))
	if err != nil {
		t.Fatalf("stdlib png.Decode failed on Adam7 output: %v", err)
	}
	for y := 0; y < 63; y++ {
		for x := 0; x < 97; x++ {
			c1 := color.NRGBAModel.Convert(origNRGBA.At(x, y)).(color.NRGBA)
			c2 := color.NRGBAModel.Convert(decInterlaced.At(x, y)).(color.NRGBA)
			if c1 != c2 {
				t.Fatalf("Adam7 pixel mismatch at (%d,%d): got %+v, want %+v", x, y, c2, c1)
			}
		}
	}

	// 2. Median-Cut Quantized + Adam7 Interlaced Paletted PNG (`colors=16&interlace=1`)
	specPal, _, _ := media.ParseImageViewSpec("?format=png&colors=16&interlace=1")
	resPal, err := eng.Transform(ctx, srcPNG, specPal)
	if err != nil {
		t.Fatalf("quantized Adam7 PNG failed: %v", err)
	}
	if resPal.Data[28] != 1 || resPal.Data[25] != 3 { // interlace=1, colorType=3 (indexed PLTE)
		t.Fatalf("expected indexed Adam7 PNG (colorType=3, interlace=1), got colorType=%d interlace=%d", resPal.Data[25], resPal.Data[28])
	}
	decPal, err := png.Decode(bytes.NewReader(resPal.Data))
	if err != nil {
		t.Fatalf("png.Decode quantized Adam7 failed: %v", err)
	}
	if palImg, ok := decPal.(*image.Paletted); !ok || len(palImg.Palette) > 16 {
		t.Fatalf("expected *image.Paletted with <= 16 colors, got %T", decPal)
	}
	// Transparent corner must remain transparent!
	if _, _, _, a := decPal.At(2, 2).RGBA(); a != 0 {
		t.Fatalf("expected transparent corner alpha=0 in quantized PNG, got %d", a)
	}

	// 3. Tiny 1x1 Adam7 edge case (where 6 of 7 Adam7 passes are empty!)
	spec1x1, _, _ := media.ParseImageViewSpec("?mode=1&w=1&h=1&format=png&interlace=1")
	res1x1, err := eng.Transform(ctx, srcPNG, spec1x1)
	if err != nil {
		t.Fatalf("1x1 Adam7 PNG failed: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(res1x1.Data)); err != nil {
		t.Fatalf("1x1 Adam7 PNG decode failed: %v", err)
	}

	// 4. Quality `q=20` vs `q=95` on JPEG
	specQ20, _, _ := media.ParseImageViewSpec("?format=jpg&q=20")
	specQ95, _, _ := media.ParseImageViewSpec("?format=jpg&q=95")
	resQ20, err := eng.Transform(ctx, srcPNG, specQ20)
	if err != nil {
		t.Fatal(err)
	}
	resQ95, err := eng.Transform(ctx, srcPNG, specQ95)
	if err != nil {
		t.Fatal(err)
	}
	if len(resQ20.Data) >= len(resQ95.Data) {
		t.Fatalf("expected q=20 JPEG (%dB) to be smaller than q=95 JPEG (%dB)", len(resQ20.Data), len(resQ95.Data))
	}

	// 5. `format=avif` fallback to WebP with `FormatFallback == "avif->webp"`
	specAVIFOut, _, _ := media.ParseImageViewSpec("?imageView2/2/w/60/format/avif")
	resAVIFOut, err := eng.Transform(ctx, srcPNG, specAVIFOut)
	if err != nil {
		t.Fatalf("format=avif fallback failed: %v", err)
	}
	if resAVIFOut.ContentType != "image/webp" || resAVIFOut.FormatFallback != "avif->webp" {
		t.Fatalf("expected image/webp + FormatFallback='avif->webp', got ct=%q fb=%q", resAVIFOut.ContentType, resAVIFOut.FormatFallback)
	}

	// 6. Animated multi-frame GIF resize + colors=8 quantization
	f1 := image.NewPaletted(image.Rect(0, 0, 80, 60), color.Palette{color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}})
	f2 := image.NewPaletted(image.Rect(0, 0, 80, 60), color.Palette{color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 0, 255}})
	var gifBuf bytes.Buffer
	if err := gif.EncodeAll(&gifBuf, &gif.GIF{
		Image: []*image.Paletted{f1, f2},
		Delay: []int{10, 10},
	}); err != nil {
		t.Fatal(err)
	}
	rawGIF := gifBuf.Bytes()
	specGIF, _, _ := media.ParseImageViewSpec("?mode=2&w=40&colors=8")
	resGIF, err := eng.Transform(ctx, media.ImageViewSource{
		Key: "anim.gif", ModTimeNano: 1, Size: int64(len(rawGIF)), Ext: ".gif",
		Load: func(_ context.Context) ([]byte, error) { return rawGIF, nil },
	}, specGIF)
	if err != nil {
		t.Fatalf("animated GIF transform failed: %v", err)
	}
	decGIF, err := gif.DecodeAll(bytes.NewReader(resGIF.Data))
	if err != nil || len(decGIF.Image) != 2 || decGIF.Config.Width != 40 || decGIF.Config.Height != 30 {
		t.Fatalf("unexpected resized animated GIF: frames=%d dims=%dx%d err=%v", len(decGIF.Image), decGIF.Config.Width, decGIF.Config.Height, err)
	}

	// 7. `.avif` Upload dimension extraction + ImageView passthrough & ignore-error=1 recovery
	_, _, _, _, pipe, _, _ := setupMediaEnv(t)
	rawAVIF := synthesizeMinimalAVIF(640, 360, 1) // 90-degree irot -> 360x640!
	avifAsset, err := pipe.Upload(ctx, "admin-1", "hero.avif", "image/avif", bytes.NewReader(rawAVIF))
	if err != nil {
		t.Fatalf("Upload .avif failed: %v", err)
	}
	if avifAsset.MIMEType != "image/avif" || avifAsset.Width != 360 || avifAsset.Height != 640 {
		t.Fatalf("expected .avif 360x640 (rotated via irot), got mime=%s %dx%d", avifAsset.MIMEType, avifAsset.Width, avifAsset.Height)
	}
	avifSrc := media.ImageViewSource{
		Key: "hero.avif", ModTimeNano: 1, Size: int64(len(rawAVIF)), Ext: ".avif",
		Load: func(_ context.Context) ([]byte, error) { return rawAVIF, nil },
	}
	// Resize on .avif without ignore-error -> error; with ignore-error=1 -> untouched passthrough!
	specAVIFResizeErr, _, _ := media.ParseImageViewSpec("?w=200")
	if _, err := eng.Transform(ctx, avifSrc, specAVIFResizeErr); err == nil {
		t.Fatalf("expected AVIF pixel resize without ignore-error to return error")
	}
	specAVIFResizeIgnore, _, _ := media.ParseImageViewSpec("?w=200&ignore-error=1")
	resAVIFPass, err := eng.Transform(ctx, avifSrc, specAVIFResizeIgnore)
	if err != nil || !resAVIFPass.PassthroughOriginal || resAVIFPass.ContentType != "image/avif" || !bytes.Equal(resAVIFPass.Data, rawAVIF) {
		t.Fatalf("expected ignore-error=1 passthrough on AVIF, got res=%+v err=%v", resAVIFPass, err)
	}

	// 8. Verify original disk file SHA-256 is 100% untouched after all transforms!
	afterBytes, err := os.ReadFile(origDiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(afterBytes) != origSHA {
		t.Fatalf("CRITICAL: source file on disk was modified during ImageView rendering!")
	}
}

func TestImageViewEngine_MetadataStripKeepListAndOrientationRewrite(t *testing.T) {
	ctx := context.Background()
	eng := media.NewImageViewEngine(8 << 20)

	// Source is 120x80 with EXIF Orientation=6 (90 CW -> upright 80x120), ICC profile, and XMP!
	rawJPG, iccBytes, xmpBytes := buildRichMetadataJPEG(t, 120, 80, 6)
	src := media.ImageViewSource{
		Key:         "camera-orient6.jpg",
		ModTimeNano: 777,
		Size:        int64(len(rawJPG)),
		Ext:         ".jpg",
		Load: func(_ context.Context) ([]byte, error) {
			return rawJPG, nil
		},
	}

	// 1. Default policy (neither strip nor meta-keep-list): strips EXIF & XMP, keeps ICC!
	for _, outFmt := range []string{"jpg", "png", "webp"} {
		specDef, _, _ := media.ParseImageViewSpec("?w=80&format=" + outFmt)
		resDef, err := eng.Transform(ctx, src, specDef)
		if err != nil {
			t.Fatalf("default transform (%s) failed: %v", outFmt, err)
		}
		if media.ContainsEXIFOrGPS(resDef.Data) {
			t.Fatalf("default policy leaked EXIF/XMP in %s output", outFmt)
		}
		if outFmt == "jpg" && !bytes.Contains(resDef.Data, iccBytes) {
			t.Fatalf("expected default policy to preserve ICC profile in jpg")
		}
		if outFmt == "webp" && !bytes.Contains(resDef.Data, iccBytes) {
			t.Fatalf("expected default policy to preserve ICCP chunk in webp")
		}
	}

	// 2. Explicit `strip`: strips ICC as well as EXIF & XMP!
	specStrip, _, _ := media.ParseImageViewSpec("?w=80&format=jpg&strip")
	resStrip, err := eng.Transform(ctx, src, specStrip)
	if err != nil {
		t.Fatal(err)
	}
	if media.ContainsEXIFOrGPS(resStrip.Data) || bytes.Contains(resStrip.Data, iccBytes) {
		t.Fatalf("strip=1 failed to remove ICC/EXIF/XMP")
	}

	// 3. `meta-keep-list=exif,icc,xmp` across JPEG, PNG, and WebP:
	//    Pixels are rotated upright (80x120 -> scaled 40x60), AND retained EXIF Orientation (0x0112) MUST be rewritten to 1!
	for _, outFmt := range []string{"jpg", "png", "webp"} {
		specKeep, _, _ := media.ParseImageViewSpec("?mode=2&w=40&format=" + outFmt + "&meta-keep-list=exif,icc,xmp")
		resKeep, err := eng.Transform(ctx, src, specKeep)
		if err != nil {
			t.Fatalf("meta-keep-list transform (%s) failed: %v", outFmt, err)
		}
		img := decodeAnyRenderedImage(t, resKeep.ContentType, resKeep.Data)
		if img.Bounds().Dx() != 40 || img.Bounds().Dy() != 60 {
			t.Fatalf("expected rotated upright 40x60 in %s, got %dx%d", outFmt, img.Bounds().Dx(), img.Bounds().Dy())
		}
		if !media.ContainsEXIFOrGPS(resKeep.Data) {
			t.Fatalf("expected EXIF/XMP to be retained in %s when meta-keep-list=exif,icc,xmp", outFmt)
		}
		if gotOrient := media.ReadEXIFOrientation(resKeep.Data); gotOrient != 1 {
			t.Fatalf("CRITICAL: expected retained EXIF Orientation rewritten to 1 to prevent double rotation in %s, got %d", outFmt, gotOrient)
		}
		if !bytes.Contains(resKeep.Data, xmpBytes) {
			t.Fatalf("expected XMP packet retained in %s", outFmt)
		}
	}
}

func TestImageViewEngine_SingleflightConcurrencyLRUEvictionAndPurge(t *testing.T) {
	ctx := context.Background()
	rawJPG, _, _ := buildRichMetadataJPEG(t, 200, 150, 1)

	// 1. 50 concurrent goroutines hitting cold cache -> Load called EXACTLY ONCE!
	eng := media.NewImageViewEngine(4 << 20)
	var loadCalls atomic.Int32
	src := media.ImageViewSource{
		Key:         "burst/hero.jpg",
		ModTimeNano: 999,
		Size:        int64(len(rawJPG)),
		Ext:         ".jpg",
		Load: func(_ context.Context) ([]byte, error) {
			loadCalls.Add(1)
			time.Sleep(25 * time.Millisecond)
			return rawJPG, nil
		},
	}
	spec, _, _ := media.ParseImageViewSpec("?imageView2/1/w/120/h/90/format/webp")

	const workers = 50
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := eng.Transform(ctx, src, spec)
			if err != nil {
				errs <- err
				return
			}
			if res.ContentType != "image/webp" || !strings.HasPrefix(res.ETag, `"`) {
				errs <- fmt.Errorf("unexpected result: %+v", res)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Transform error: %v", err)
	}
	if got := loadCalls.Load(); got != 1 {
		t.Fatalf("expected singleflight + LRU to call Load exactly 1 time across %d goroutines, got %d", workers, got)
	}

	// Warm hit does not call Load again
	if _, err := eng.Transform(ctx, src, spec); err != nil {
		t.Fatal(err)
	}
	if got := loadCalls.Load(); got != 1 {
		t.Fatalf("expected warm cache hit to keep Load count at 1, got %d", got)
	}

	// Purge invalidates cache -> next Transform reloads cleanly
	eng.Purge()
	if _, err := eng.Transform(ctx, src, spec); err != nil {
		t.Fatal(err)
	}
	if got := loadCalls.Load(); got != 2 {
		t.Fatalf("expected Load count 2 after Purge(), got %d", got)
	}

	// 2. Byte-bounded LRU eviction test
	// Measure one entry's rendered size, then size cache to hold ~3 entries (while maxEntryBytes = cache/8 -> use 10 entries in a 9*entrySize cache!)
	sampleRes, err := eng.Transform(ctx, src, spec)
	if err != nil {
		t.Fatal(err)
	}
	entryLen := int64(len(sampleRes.Data))
	smallCap := entryLen * 9 // maxEntryBytes = 1.125 * entryLen, holds at most 9 entries!
	smallEng := media.NewImageViewEngine(smallCap)

	var evictLoads atomic.Int32
	makeSrc := func(idx int) media.ImageViewSource {
		return media.ImageViewSource{
			Key:         fmt.Sprintf("item-%d.jpg", idx),
			ModTimeNano: 1,
			Size:        int64(len(rawJPG)),
			Ext:         ".jpg",
			Load: func(_ context.Context) ([]byte, error) {
				evictLoads.Add(1)
				return rawJPG, nil
			},
		}
	}
	// Insert 12 distinct keys -> oldest keys (0, 1, 2) must be evicted while newest key (11) stays cached!
	for i := 0; i < 12; i++ {
		if _, err := smallEng.Transform(ctx, makeSrc(i), spec); err != nil {
			t.Fatal(err)
		}
	}
	beforeHit := evictLoads.Load()
	// Key 11 should be a cache hit
	if _, err := smallEng.Transform(ctx, makeSrc(11), spec); err != nil {
		t.Fatal(err)
	}
	if evictLoads.Load() != beforeHit {
		t.Fatalf("expected most-recent entry 11 to hit LRU cache")
	}
	// Key 0 should have been evicted -> triggers a fresh Load
	if _, err := smallEng.Transform(ctx, makeSrc(0), spec); err != nil {
		t.Fatal(err)
	}
	if evictLoads.Load() != beforeHit+1 {
		t.Fatalf("expected oldest entry 0 to be evicted when LRU byte budget exceeded")
	}
}
