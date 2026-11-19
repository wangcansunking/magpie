//go:build !dev

package wan

func sdkHelper() ([]byte, error) { return sdkHost, nil }
