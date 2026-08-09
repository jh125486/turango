package example_test

import (
	"testing"

	"github.com/jh125486/turango/example"
)

// The tests in this file are the honest kind: written from the doc comments,
// covering the cases a reviewer would ask about, and still leaving gaps that
// only show up once something actually changes the code under them. Running
// turango against this package is what makes those gaps visible — see
// README.md.

func TestSubtotal(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		items []example.Item
		want  int
	}{
		"no items": {
			want: 0,
		},
		"one line": {
			items: []example.Item{{Name: "mug", UnitCents: 1250, Quantity: 2}},
			want:  2500,
		},
		"several lines": {
			items: []example.Item{
				{Name: "mug", UnitCents: 1250, Quantity: 2},
				{Name: "spoon", UnitCents: 300, Quantity: 3},
			},
			want: 3400,
		},
		"a line with no quantity contributes nothing": {
			items: []example.Item{
				{Name: "mug", UnitCents: 1250, Quantity: 2},
				{Name: "out of stock", UnitCents: 9999, Quantity: 0},
			},
			want: 2500,
		},
		"a negative quantity is not a refund": {
			items: []example.Item{
				{Name: "mug", UnitCents: 1250, Quantity: 2},
				{Name: "returned", UnitCents: 500, Quantity: -1},
			},
			want: 2500,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := example.Subtotal(tt.items); got != tt.want {
				t.Errorf("Subtotal() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDiscountCents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		subtotal int
		coupon   string
		member   bool
		want     int
	}{
		"no coupon":                  {subtotal: 2000, want: 0},
		"unknown coupon":             {subtotal: 2000, coupon: "NOPE", want: 0},
		"half off":                   {subtotal: 2000, coupon: example.CouponHalfOff, want: 1000},
		"ten off a large order":      {subtotal: 2500, coupon: example.CouponTenOff, want: 1000},
		"ten off a small order":      {subtotal: 999, coupon: example.CouponTenOff, want: 999},
		"member discount for member": {subtotal: 2000, coupon: example.CouponMember, member: true, want: 200},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := example.DiscountCents(tt.subtotal, tt.coupon, tt.member); got != tt.want {
				t.Errorf("DiscountCents(%d, %q, %v) = %d, want %d",
					tt.subtotal, tt.coupon, tt.member, got, tt.want)
			}
		})
	}
}

func TestShippingCents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		subtotal  int
		expedited bool
		want      int
	}{
		"small order pays standard shipping": {subtotal: 4000, want: 599},
		"large order ships free":             {subtotal: 6000, want: 0},
		"expedited is never free":            {subtotal: 6000, expedited: true, want: 1799},
		"small expedited order":              {subtotal: 4000, expedited: true, want: 1799},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := example.ShippingCents(tt.subtotal, tt.expedited); got != tt.want {
				t.Errorf("ShippingCents(%d, %v) = %d, want %d", tt.subtotal, tt.expedited, got, tt.want)
			}
		})
	}
}

func TestTotal(t *testing.T) {
	t.Parallel()

	basket := []example.Item{{Name: "mug", UnitCents: 3000, Quantity: 2}}

	tests := map[string]struct {
		order example.Order
		want  int
	}{
		"free shipping, no coupon": {
			order: example.Order{Items: basket},
			want:  6000,
		},
		"a discount can push the order back under the free-shipping threshold": {
			order: example.Order{Items: basket, Coupon: example.CouponHalfOff},
			want:  3599,
		},
		"expedited always pays": {
			order: example.Order{Items: basket, Expedited: true},
			want:  7799,
		},
		"an empty order still pays shipping": {
			order: example.Order{},
			want:  599,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := example.Total(tt.order); got != tt.want {
				t.Errorf("Total(%+v) = %d, want %d", tt.order, got, tt.want)
			}
		})
	}
}

func TestRestockingFeeCents(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		subtotal int
		want     int
	}{
		"nothing returned": {subtotal: 0, want: 0},
		"a negative order": {subtotal: -100, want: 0},
		"five percent":     {subtotal: 2000, want: 100},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := example.RestockingFeeCents(tt.subtotal); got != tt.want {
				t.Errorf("RestockingFeeCents(%d) = %d, want %d", tt.subtotal, got, tt.want)
			}
		})
	}
}
