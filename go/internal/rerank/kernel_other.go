//go:build !amd64 || purego

package rerank

const useFMA = false

func kernel2x4(x0, x1, w0, w1, w2, w3 []float32) [8]float32 {
	return kernel2x4Go(x0, x1, w0, w1, w2, w3)
}
