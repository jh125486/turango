// Minimal stand-in for internal/asan (the "not compiled with -asan" case),
// for this isolated mutation-testing fixture.
package asan

import "unsafe"

const Enabled = false

func Read(addr unsafe.Pointer, len uintptr) {}
func Write(addr unsafe.Pointer, len uintptr) {}
