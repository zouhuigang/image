// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:generate go run gen.go
//go:generate asmfmt -w acc_amd64.s

// asmfmt is https://github.com/klauspost/asmfmt

// Package vector provides a rasterizer for 2-D vector graphics.
package vector // import "golang.org/x/image/vector"

// The rasterizer's design follows
// https://medium.com/@raphlinus/inside-the-fastest-font-renderer-in-the-world-75ae5270c445
//
// Proof of concept code is in
// https://github.com/google/font-go
//
// See also:
// http://nothings.org/gamedev/rasterize/
// http://projects.tuxee.net/cl-vectors/section-the-cl-aa-algorithm
// https://people.gnome.org/~mathieu/libart/internals.html#INTERNALS-SCANLINE

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/math/f32"
)

// floatingPointMathThreshold is the width or hight above which the rasterizer
// chooses to used floating point math instead of fixed point math.
//
// Both implementations of line segmentation rasterization (see raster_fixed.go
// and raster_floating.go) implement the same algorithm (in ideal, infinite
// precision math) but they perform differently in practice. The fixed point
// math version is roughtly 1.25x faster (on GOARCH=amd64) on the benchmarks,
// but at sufficiently large scales, the computations will overflow and hence
// show rendering artifacts. The floating point math version has more
// consistent quality over larger scales, but it is significantly slower.
//
// This constant determines when to use the faster implementation and when to
// use the better quality implementation.
//
// The rationale for this particular value is that TestRasterizePolygon in
// vector_test.go checks the rendering quality of polygon edges at various
// angles, inscribed in a circle of diameter 2048. It may be that a higher
// value would still produce acceptable quality, but 2048 seems to work.
const floatingPointMathThreshold = 2048

func midPoint(p, q f32.Vec2) f32.Vec2 {
	return f32.Vec2{
		(p[0] + q[0]) * 0.5,
		(p[1] + q[1]) * 0.5,
	}
}

func lerp(t float32, p, q f32.Vec2) f32.Vec2 {
	return f32.Vec2{
		p[0] + t*(q[0]-p[0]),
		p[1] + t*(q[1]-p[1]),
	}
}

func clamp(i, width int32) uint {
	if i < 0 {
		return 0
	}
	if i < width {
		return uint(i)
	}
	return uint(width)
}

// NewRasterizer returns a new Rasterizer whose rendered mask image is bounded
// by the given width and height.
func NewRasterizer(w, h int) *Rasterizer {
	z := &Rasterizer{}
	z.Reset(w, h)
	return z
}

// Raster is a 2-D vector graphics rasterizer.
//
// The zero value is usable, in that it is a Rasterizer whose rendered mask
// image has zero width and zero height. Call Reset to change its bounds.
type Rasterizer struct {
	// bufXxx are buffers of float32 or uint32 values, holding either the
	// individual or cumulative area values.
	//
	// We don't actually need both values at any given time, and to conserve
	// memory, the integration of the individual to the cumulative could modify
	// the buffer in place. In other words, we could use a single buffer, say
	// of type []uint32, and add some math.Float32bits and math.Float32frombits
	// calls to satisfy the compiler's type checking. As of Go 1.7, though,
	// there is a performance penalty between:
	//	bufF32[i] += x
	// and
	//	bufU32[i] = math.Float32bits(x + math.Float32frombits(bufU32[i]))
	//
	// See golang.org/issue/17220 for some discussion.
	bufF32 []float32
	bufU32 []uint32

	useFloatingPointMath bool

	size  image.Point
	first f32.Vec2
	pen   f32.Vec2

	// DrawOp is the operator used for the Draw method.
	//
	// The zero value is draw.Over.
	DrawOp draw.Op

	// TODO: an exported field equivalent to the mask point in the
	// draw.DrawMask function in the stdlib image/draw package?
}

// Reset resets a Rasterizer as if it was just returned by NewRasterizer.
//
// This includes setting z.DrawOp to draw.Over.
func (z *Rasterizer) Reset(w, h int) {
	z.size = image.Point{w, h}
	z.first = f32.Vec2{}
	z.pen = f32.Vec2{}
	z.DrawOp = draw.Over

	z.setUseFloatingPointMath(w > floatingPointMathThreshold || h > floatingPointMathThreshold)
}

func (z *Rasterizer) setUseFloatingPointMath(b bool) {
	z.useFloatingPointMath = b

	// Make z.bufF32 or z.bufU32 large enough to hold width * height samples.
	if z.useFloatingPointMath {
		if n := z.size.X * z.size.Y; n > cap(z.bufF32) {
			z.bufF32 = make([]float32, n)
		} else {
			z.bufF32 = z.bufF32[:n]
			for i := range z.bufF32 {
				z.bufF32[i] = 0
			}
		}
	} else {
		if n := z.size.X * z.size.Y; n > cap(z.bufU32) {
			z.bufU32 = make([]uint32, n)
		} else {
			z.bufU32 = z.bufU32[:n]
			for i := range z.bufU32 {
				z.bufU32[i] = 0
			}
		}
	}
}

// Size returns the width and height passed to NewRasterizer or Reset.
func (z *Rasterizer) Size() image.Point {
	return z.size
}

// Bounds returns the rectangle from (0, 0) to the width and height passed to
// NewRasterizer or Reset.
func (z *Rasterizer) Bounds() image.Rectangle {
	return image.Rectangle{Max: z.size}
}

// Pen returns the location of the path-drawing pen: the last argument to the
// most recent XxxTo call.
func (z *Rasterizer) Pen() f32.Vec2 {
	return z.pen
}

// ClosePath closes the current path.
func (z *Rasterizer) ClosePath() {
	z.LineTo(z.first)
}

// MoveTo starts a new path and moves the pen to a.
//
// The coordinates are allowed to be out of the Rasterizer's bounds.
func (z *Rasterizer) MoveTo(a f32.Vec2) {
	z.first = a
	z.pen = a
}

// LineTo adds a line segment, from the pen to b, and moves the pen to b.
//
// The coordinates are allowed to be out of the Rasterizer's bounds.
func (z *Rasterizer) LineTo(b f32.Vec2) {
	if z.useFloatingPointMath {
		z.floatingLineTo(b)
	} else {
		z.fixedLineTo(b)
	}
}

// QuadTo adds a quadratic Bézier segment, from the pen via b to c, and moves
// the pen to c.
//
// The coordinates are allowed to be out of the Rasterizer's bounds.
func (z *Rasterizer) QuadTo(b, c f32.Vec2) {
	a := z.pen
	devsq := devSquared(a, b, c)
	if devsq >= 0.333 {
		const tol = 3
		n := 1 + int(math.Sqrt(math.Sqrt(tol*float64(devsq))))
		t, nInv := float32(0), 1/float32(n)
		for i := 0; i < n-1; i++ {
			t += nInv
			ab := lerp(t, a, b)
			bc := lerp(t, b, c)
			z.LineTo(lerp(t, ab, bc))
		}
	}
	z.LineTo(c)
}

// CubeTo adds a cubic Bézier segment, from the pen via b and c to d, and moves
// the pen to d.
//
// The coordinates are allowed to be out of the Rasterizer's bounds.
func (z *Rasterizer) CubeTo(b, c, d f32.Vec2) {
	a := z.pen
	devsq := devSquared(a, b, d)
	if devsqAlt := devSquared(a, c, d); devsq < devsqAlt {
		devsq = devsqAlt
	}
	if devsq >= 0.333 {
		const tol = 3
		n := 1 + int(math.Sqrt(math.Sqrt(tol*float64(devsq))))
		t, nInv := float32(0), 1/float32(n)
		for i := 0; i < n-1; i++ {
			t += nInv
			ab := lerp(t, a, b)
			bc := lerp(t, b, c)
			cd := lerp(t, c, d)
			abc := lerp(t, ab, bc)
			bcd := lerp(t, bc, cd)
			z.LineTo(lerp(t, abc, bcd))
		}
	}
	z.LineTo(d)
}

// devSquared returns a measure of how curvy the sequnce a to b to c is. It
// determines how many line segments will approximate a Bézier curve segment.
//
// http://lists.nongnu.org/archive/html/freetype-devel/2016-08/msg00080.html
// gives the rationale for this evenly spaced heuristic instead of a recursive
// de Casteljau approach:
//
// The reason for the subdivision by n is that I expect the "flatness"
// computation to be semi-expensive (it's done once rather than on each
// potential subdivision) and also because you'll often get fewer subdivisions.
// Taking a circular arc as a simplifying assumption (ie a spherical cow),
// where I get n, a recursive approach would get 2^⌈lg n⌉, which, if I haven't
// made any horrible mistakes, is expected to be 33% more in the limit.
func devSquared(a, b, c f32.Vec2) float32 {
	devx := a[0] - 2*b[0] + c[0]
	devy := a[1] - 2*b[1] + c[1]
	return devx*devx + devy*devy
}

// Draw implements the Drawer interface from the standard library's image/draw
// package.
//
// The vector paths previously added via the XxxTo calls become the mask for
// drawing src onto dst.
func (z *Rasterizer) Draw(dst draw.Image, r image.Rectangle, src image.Image, sp image.Point) {
	// TODO: adjust r and sp (and mp?) if src.Bounds() doesn't contain
	// r.Add(sp.Sub(r.Min)).

	if src, ok := src.(*image.Uniform); ok {
		srcR, srcG, srcB, srcA := src.RGBA()
		switch dst := dst.(type) {
		case *image.Alpha:
			// Fast path for glyph rendering.
			if srcA == 0xffff {
				if z.DrawOp == draw.Over {
					z.rasterizeDstAlphaSrcOpaqueOpOver(dst, r)
				} else {
					z.rasterizeDstAlphaSrcOpaqueOpSrc(dst, r)
				}
				return
			}
		case *image.RGBA:
			if z.DrawOp == draw.Over {
				z.rasterizeDstRGBASrcUniformOpOver(dst, r, srcR, srcG, srcB, srcA)
			} else {
				z.rasterizeDstRGBASrcUniformOpSrc(dst, r, srcR, srcG, srcB, srcA)
			}
			return
		}
	}

	if z.DrawOp == draw.Over {
		z.rasterizeOpOver(dst, r, src, sp)
	} else {
		z.rasterizeOpSrc(dst, r, src, sp)
	}
}

func (z *Rasterizer) accumulateMask() {
	if z.useFloatingPointMath {
		if n := z.size.X * z.size.Y; n > cap(z.bufU32) {
			z.bufU32 = make([]uint32, n)
		} else {
			z.bufU32 = z.bufU32[:n]
		}
		if haveFloatingAccumulateSIMD {
			floatingAccumulateMaskSIMD(z.bufU32, z.bufF32)
		} else {
			floatingAccumulateMask(z.bufU32, z.bufF32)
		}
	} else {
		if haveFixedAccumulateSIMD {
			fixedAccumulateMaskSIMD(z.bufU32)
		} else {
			fixedAccumulateMask(z.bufU32)
		}
	}
}

func (z *Rasterizer) rasterizeDstAlphaSrcOpaqueOpOver(dst *image.Alpha, r image.Rectangle) {
	// TODO: non-zero vs even-odd winding?
	if r == dst.Bounds() && r == z.Bounds() {
		// We bypass the z.accumulateMask step and convert straight from
		// z.bufF32 or z.bufU32 to dst.Pix.
		if z.useFloatingPointMath {
			if haveFloatingAccumulateSIMD {
				floatingAccumulateOpOverSIMD(dst.Pix, z.bufF32)
			} else {
				floatingAccumulateOpOver(dst.Pix, z.bufF32)
			}
		} else {
			if haveFixedAccumulateSIMD {
				fixedAccumulateOpOverSIMD(dst.Pix, z.bufU32)
			} else {
				fixedAccumulateOpOver(dst.Pix, z.bufU32)
			}
		}
		return
	}

	z.accumulateMask()
	pix := dst.Pix[dst.PixOffset(r.Min.X, r.Min.Y):]
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			ma := z.bufU32[y*z.size.X+x]
			i := y*dst.Stride + x

			// This formula is like rasterizeOpOver's, simplified for the
			// concrete dst type and opaque src assumption.
			a := 0xffff - ma
			pix[i] = uint8((uint32(pix[i])*0x101*a/0xffff + ma) >> 8)
		}
	}
}

func (z *Rasterizer) rasterizeDstAlphaSrcOpaqueOpSrc(dst *image.Alpha, r image.Rectangle) {
	// TODO: non-zero vs even-odd winding?
	if r == dst.Bounds() && r == z.Bounds() {
		// We bypass the z.accumulateMask step and convert straight from
		// z.bufF32 or z.bufU32 to dst.Pix.
		if z.useFloatingPointMath {
			if haveFloatingAccumulateSIMD {
				floatingAccumulateOpSrcSIMD(dst.Pix, z.bufF32)
			} else {
				floatingAccumulateOpSrc(dst.Pix, z.bufF32)
			}
		} else {
			if haveFixedAccumulateSIMD {
				fixedAccumulateOpSrcSIMD(dst.Pix, z.bufU32)
			} else {
				fixedAccumulateOpSrc(dst.Pix, z.bufU32)
			}
		}
		return
	}

	z.accumulateMask()
	pix := dst.Pix[dst.PixOffset(r.Min.X, r.Min.Y):]
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			ma := z.bufU32[y*z.size.X+x]

			// This formula is like rasterizeOpSrc's, simplified for the
			// concrete dst type and opaque src assumption.
			pix[y*dst.Stride+x] = uint8(ma >> 8)
		}
	}
}

func (z *Rasterizer) rasterizeDstRGBASrcUniformOpOver(dst *image.RGBA, r image.Rectangle, sr, sg, sb, sa uint32) {
	z.accumulateMask()
	pix := dst.Pix[dst.PixOffset(r.Min.X, r.Min.Y):]
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			ma := z.bufU32[y*z.size.X+x]

			// This formula is like rasterizeOpOver's, simplified for the
			// concrete dst type and uniform src assumption.
			a := 0xffff - (sa * ma / 0xffff)
			i := y*dst.Stride + 4*x
			pix[i+0] = uint8(((uint32(pix[i+0])*0x101*a + sr*ma) / 0xffff) >> 8)
			pix[i+1] = uint8(((uint32(pix[i+1])*0x101*a + sg*ma) / 0xffff) >> 8)
			pix[i+2] = uint8(((uint32(pix[i+2])*0x101*a + sb*ma) / 0xffff) >> 8)
			pix[i+3] = uint8(((uint32(pix[i+3])*0x101*a + sa*ma) / 0xffff) >> 8)
		}
	}
}

func (z *Rasterizer) rasterizeDstRGBASrcUniformOpSrc(dst *image.RGBA, r image.Rectangle, sr, sg, sb, sa uint32) {
	z.accumulateMask()
	pix := dst.Pix[dst.PixOffset(r.Min.X, r.Min.Y):]
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			ma := z.bufU32[y*z.size.X+x]

			// This formula is like rasterizeOpSrc's, simplified for the
			// concrete dst type and uniform src assumption.
			i := y*dst.Stride + 4*x
			pix[i+0] = uint8((sr * ma / 0xffff) >> 8)
			pix[i+1] = uint8((sg * ma / 0xffff) >> 8)
			pix[i+2] = uint8((sb * ma / 0xffff) >> 8)
			pix[i+3] = uint8((sa * ma / 0xffff) >> 8)
		}
	}
}

func (z *Rasterizer) rasterizeOpOver(dst draw.Image, r image.Rectangle, src image.Image, sp image.Point) {
	z.accumulateMask()
	out := color.RGBA64{}
	outc := color.Color(&out)
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			sr, sg, sb, sa := src.At(sp.X+x, sp.Y+y).RGBA()
			ma := z.bufU32[y*z.size.X+x]

			// This algorithm comes from the standard library's image/draw
			// package.
			dr, dg, db, da := dst.At(r.Min.X+x, r.Min.Y+y).RGBA()
			a := 0xffff - (sa * ma / 0xffff)
			out.R = uint16((dr*a + sr*ma) / 0xffff)
			out.G = uint16((dg*a + sg*ma) / 0xffff)
			out.B = uint16((db*a + sb*ma) / 0xffff)
			out.A = uint16((da*a + sa*ma) / 0xffff)

			dst.Set(r.Min.X+x, r.Min.Y+y, outc)
		}
	}
}

func (z *Rasterizer) rasterizeOpSrc(dst draw.Image, r image.Rectangle, src image.Image, sp image.Point) {
	z.accumulateMask()
	out := color.RGBA64{}
	outc := color.Color(&out)
	for y, y1 := 0, r.Max.Y-r.Min.Y; y < y1; y++ {
		for x, x1 := 0, r.Max.X-r.Min.X; x < x1; x++ {
			sr, sg, sb, sa := src.At(sp.X+x, sp.Y+y).RGBA()
			ma := z.bufU32[y*z.size.X+x]

			// This algorithm comes from the standard library's image/draw
			// package.
			out.R = uint16(sr * ma / 0xffff)
			out.G = uint16(sg * ma / 0xffff)
			out.B = uint16(sb * ma / 0xffff)
			out.A = uint16(sa * ma / 0xffff)

			dst.Set(r.Min.X+x, r.Min.Y+y, outc)
		}
	}
}
