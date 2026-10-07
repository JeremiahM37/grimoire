//go:build amd64 && !purego

#include "textflag.h"

// func kernel2x4FMA(x0, x1, w0, w1, w2, w3 *float32, n int, out *[8]float32)
//
// The 2-row × 4-column block of dot products, 8 lanes at a time with AVX2
// FMA: Y0-Y3 accumulate x0·w0..w3, Y4-Y7 accumulate x1·w0..w3. n must be a
// multiple of 8. Each accumulator is reduced horizontally once at the end.
TEXT ·kernel2x4FMA(SB), NOSPLIT, $0-64
	MOVQ x0+0(FP), SI
	MOVQ x1+8(FP), DI
	MOVQ w0+16(FP), R8
	MOVQ w1+24(FP), R9
	MOVQ w2+32(FP), R10
	MOVQ w3+40(FP), R11
	MOVQ n+48(FP), CX
	MOVQ out+56(FP), DX

	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3
	VXORPS Y4, Y4, Y4
	VXORPS Y5, Y5, Y5
	VXORPS Y6, Y6, Y6
	VXORPS Y7, Y7, Y7

	SHLQ $2, CX // bytes
	XORQ AX, AX
	TESTQ CX, CX
	JE   reduce

loop:
	VMOVUPS (SI)(AX*1), Y8
	VMOVUPS (DI)(AX*1), Y9
	VMOVUPS (R8)(AX*1), Y10
	VMOVUPS (R9)(AX*1), Y11
	VMOVUPS (R10)(AX*1), Y12
	VMOVUPS (R11)(AX*1), Y13
	VFMADD231PS Y10, Y8, Y0
	VFMADD231PS Y11, Y8, Y1
	VFMADD231PS Y12, Y8, Y2
	VFMADD231PS Y13, Y8, Y3
	VFMADD231PS Y10, Y9, Y4
	VFMADD231PS Y11, Y9, Y5
	VFMADD231PS Y12, Y9, Y6
	VFMADD231PS Y13, Y9, Y7
	ADDQ $32, AX
	CMPQ AX, CX
	JLT  loop

reduce:
	// hadd(hadd(a,b), hadd(c,d)) leaves [Σa Σb Σc Σd] in each 128-bit lane
	VHADDPS Y1, Y0, Y0
	VHADDPS Y3, Y2, Y2
	VHADDPS Y2, Y0, Y0
	VEXTRACTF128 $1, Y0, X1
	VADDPS X1, X0, X0
	VMOVUPS X0, (DX)

	VHADDPS Y5, Y4, Y4
	VHADDPS Y7, Y6, Y6
	VHADDPS Y6, Y4, Y4
	VEXTRACTF128 $1, Y4, X5
	VADDPS X5, X4, X4
	VMOVUPS X4, 16(DX)

	VZEROUPPER
	RET
