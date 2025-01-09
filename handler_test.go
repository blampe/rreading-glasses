package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPathToID(t *testing.T) {
	tests := []struct {
		given   string
		want    int64
		wantErr error
	}{
		{
			given: "/book/show/27362503-it-ends-with-us",
			want:  27362503,
		},
		{
			given: "/book/show/7244.The_Poisonwood_Bible",
			want:  7244,
		},
		{
			given: "/work/1842237",
			want:  1842237,
		},
		{
			given: "/book/show/15704307-saga-volume-1",
			want:  15704307,
		},
		{
			given: "https://www.example.com/book/show/218467.Lucifer_s_Hammer",
			want:  218467,
		},
		{
			given:   "/author/-1234",
			want:    -1234,
			wantErr: errBadRequest,
		},
	}

	for _, tt := range tests {
		actual, err := pathToID(tt.given)
		assert.ErrorIs(t, err, tt.wantErr)
		assert.Equal(t, tt.want, actual)
	}
}
