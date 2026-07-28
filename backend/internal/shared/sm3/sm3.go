// Package sm3 implements the SM3 cryptographic hash algorithm (GM/T 0004-2012).
package sm3

import (
	"encoding/binary"
	"hash"
	"math/bits"
)

const (
	// Size is the size of an SM3 checksum in bytes.
	Size = 32
	// BlockSize is the block size of SM3 in bytes.
	BlockSize = 64
)

var initialState = [8]uint32{
	0x7380166f, 0x4914b2b9, 0x172442d7, 0xda8a0600,
	0xa96f30bc, 0x163138aa, 0xe38dee4d, 0xb0fb0e4e,
}

type digest struct {
	h   [8]uint32
	x   [BlockSize]byte
	nx  int
	len uint64
}

// New returns a new SM3 hash.Hash.
func New() hash.Hash {
	d := new(digest)
	d.Reset()
	return d
}

// Sum returns the SM3 checksum of data.
func Sum(data []byte) [Size]byte {
	d := new(digest)
	d.Reset()
	_, _ = d.Write(data)
	return d.checkSum()
}

func (d *digest) Reset() {
	d.h = initialState
	d.nx = 0
	d.len = 0
}

func (d *digest) Size() int      { return Size }
func (d *digest) BlockSize() int { return BlockSize }

func (d *digest) Write(p []byte) (int, error) {
	nn := len(p)
	d.len += uint64(nn)
	if d.nx > 0 {
		n := copy(d.x[d.nx:], p)
		d.nx += n
		if d.nx == BlockSize {
			d.block(d.x[:])
			d.nx = 0
		}
		p = p[n:]
	}
	for len(p) >= BlockSize {
		d.block(p[:BlockSize])
		p = p[BlockSize:]
	}
	if len(p) > 0 {
		d.nx = copy(d.x[:], p)
	}
	return nn, nil
}

func (d *digest) Sum(in []byte) []byte {
	d0 := *d
	s := d0.checkSum()
	return append(in, s[:]...)
}

func (d *digest) checkSum() [Size]byte {
	bitLen := d.len << 3
	_, _ = d.Write([]byte{0x80})
	for d.nx != 56 {
		_, _ = d.Write([]byte{0})
	}
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], bitLen)
	_, _ = d.Write(length[:])
	if d.nx != 0 {
		panic("sm3: invalid padding")
	}
	var out [Size]byte
	for i, v := range d.h {
		binary.BigEndian.PutUint32(out[i*4:], v)
	}
	return out
}

func (d *digest) block(p []byte) {
	var w [68]uint32
	var w1 [64]uint32
	for i := 0; i < 16; i++ {
		w[i] = binary.BigEndian.Uint32(p[i*4:])
	}
	for i := 16; i < 68; i++ {
		w[i] = p1(w[i-16]^w[i-9]^bits.RotateLeft32(w[i-3], 15)) ^ bits.RotateLeft32(w[i-13], 7) ^ w[i-6]
	}
	for i := 0; i < 64; i++ {
		w1[i] = w[i] ^ w[i+4]
	}
	a, b, c, dd := d.h[0], d.h[1], d.h[2], d.h[3]
	e, f, g, h := d.h[4], d.h[5], d.h[6], d.h[7]
	for i := 0; i < 64; i++ {
		t := uint32(0x7a879d8a)
		if i < 16 {
			t = 0x79cc4519
		}
		ss1 := bits.RotateLeft32(bits.RotateLeft32(a, 12)+e+bits.RotateLeft32(t, i), 7)
		ss2 := ss1 ^ bits.RotateLeft32(a, 12)
		var ff, gg uint32
		if i < 16 {
			ff = a ^ b ^ c
			gg = e ^ f ^ g
		} else {
			ff = (a & b) | (a & c) | (b & c)
			gg = (e & f) | (^e & g)
		}
		tt1 := ff + dd + ss2 + w1[i]
		tt2 := gg + h + ss1 + w[i]
		dd = c
		c = bits.RotateLeft32(b, 9)
		b = a
		a = tt1
		h = g
		g = bits.RotateLeft32(f, 19)
		f = e
		e = p0(tt2)
	}
	d.h[0] ^= a
	d.h[1] ^= b
	d.h[2] ^= c
	d.h[3] ^= dd
	d.h[4] ^= e
	d.h[5] ^= f
	d.h[6] ^= g
	d.h[7] ^= h
}

func p0(x uint32) uint32 { return x ^ bits.RotateLeft32(x, 9) ^ bits.RotateLeft32(x, 17) }
func p1(x uint32) uint32 { return x ^ bits.RotateLeft32(x, 15) ^ bits.RotateLeft32(x, 23) }
