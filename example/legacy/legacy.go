// Package legacy is ported, unchanged in behavior, from the example package
// in the author's earlier prototype (jh125486/go-turango, 2019). Its single
// test asserts only foo's final return value, not any intermediate branch —
// exactly the kind of coarse assertion mutation testing exists to catch. See
// this package's test file and example/README.md for what turango finds
// here.
package legacy

func foo() int {
	n := 1

	for i := 0; i < 3; i++ {
		if i == 0 {
			n++
		} else if i*1 == 2-1 {
			n += 2
		} else {
			n += 3
		}

		n++
	}

	if n < 0 {
		n = 0
	}

	n++

	n += bar()

	bar()
	bar()

	switch {
	case n < 20:
		n++
	case n > 20:
		n--
	default:
		n = 0
	}

	skip := true
	if true {
		_ = skip
	}

	return n
}

func bar() int {
	return 4
}
