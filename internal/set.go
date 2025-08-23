package internal

import "maps"

type set[T comparable] map[T]struct{}

func newSet[T comparable](ts ...T) set[T] {
	s := set[T]{}
	for _, t := range ts {
		s[t] = struct{}{}
	}
	return s
}

func union[T comparable, S set[T]](x S, y S) S {
	r := maps.Clone(x)
	maps.Copy(r, y)
	return r
}
