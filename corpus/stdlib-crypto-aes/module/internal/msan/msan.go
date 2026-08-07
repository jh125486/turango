// Minimal stand-in for internal/msan (the "not compiled with -msan" case),
// for this isolated mutation-testing fixture.
package msan

import "unsafe"

const Enabled = false

func Read(addr unsafe.Pointer, sz uintptr) {}
func Write(addr unsafe.Pointer, sz uintptr) {}
