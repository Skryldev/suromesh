package tl

// Marshal encodes obj into a freshly allocated buffer. Convenience for tests
// and tools; hot paths should reuse a Writer.
func Marshal(obj Object) ([]byte, error) {
	w := NewWriter(256)
	if err := obj.Encode(w); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

// Unmarshal decodes a complete object (constructor ID included) from b.
func Unmarshal(b []byte) (Object, error) {
	r := NewReader(b)
	obj, err := DecodeObject(r)
	if err != nil {
		return nil, err
	}
	return obj, nil
}
