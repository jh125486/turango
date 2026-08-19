package example

// Money is in integer cents throughout: the arithmetic below is exact, which
// keeps the mutants interesting (a surviving mutant means a missing assertion,
// never a rounding coincidence).
const (
	// freeShippingCents is the discounted subtotal at which standard shipping
	// stops being charged.
	freeShippingCents = 5000

	// standardShippingCents is the flat rate for standard delivery.
	standardShippingCents = 599

	// expeditedSurchargeCents is added on top of the standard rate for
	// expedited delivery, which is never free.
	expeditedSurchargeCents = 1200

	// restockingFeePercent is the share of a returned order's value the
	// customer forfeits.
	restockingFeePercent = 5
)

// Coupon codes accepted at checkout.
const (
	CouponHalfOff = "HALFOFF"
	CouponTenOff  = "TENOFF"
	CouponMember  = "MEMBER"
)

// Item is one line of an order: a named product, its unit price, and how many
// of it were ordered.
type Item struct {
	Name      string
	UnitCents int
	Quantity  int
}

// Order is a basket of items together with the choices made at checkout.
type Order struct {
	Items     []Item
	Coupon    string
	Expedited bool
	Member    bool
}

// Subtotal is the undiscounted value of every line in the order.
//
// A line with a non-positive quantity contributes nothing rather than
// subtracting: a refund is a separate order, not a negative line.
func Subtotal(items []Item) int {
	subtotal := 0

	for _, item := range items {
		if item.Quantity <= 0 {
			continue
		}

		subtotal += item.UnitCents * item.Quantity
	}

	return subtotal
}

// DiscountCents is the amount coupon takes off subtotal.
//
// An unknown or empty coupon is worth nothing. A discount never exceeds the
// subtotal itself, which is [Total]'s job to enforce, not this function's.
func DiscountCents(subtotal int, coupon string, member bool) int {
	switch coupon {
	case CouponHalfOff:
		return subtotal / 2

	case CouponTenOff:
		if subtotal < 1000 {
			return subtotal
		}

		return 1000

	case CouponMember:
		if !member {
			return 0
		}

		return subtotal / 10

	default:
		return 0
	}
}

// ShippingCents is what delivery costs on an order worth subtotal.
//
// Standard delivery is free above [freeShippingCents]; expedited delivery is
// always charged, because the surcharge pays for the courier either way.
func ShippingCents(subtotal int, expedited bool) int {
	if subtotal >= freeShippingCents && !expedited {
		return 0
	}

	cost := standardShippingCents

	if expedited {
		cost += expeditedSurchargeCents
	}

	return cost
}

// Total is what the customer pays: the subtotal, less any discount, plus
// shipping on the discounted amount.
func Total(order Order) int {
	subtotal := Subtotal(order.Items)

	discount := min(DiscountCents(subtotal, order.Coupon, order.Member), subtotal)

	payable := subtotal - discount

	return payable + ShippingCents(payable, order.Expedited)
}

// RestockingFeeCents is what a customer forfeits when they return an order
// worth subtotal.
func RestockingFeeCents(subtotal int) int {
	if subtotal <= 0 {
		return 0
	}

	//nomutant: the fee is a contractual percentage, not logic — mutating the arithmetic only re-asserts the number in a second place
	return subtotal * restockingFeePercent / 100
}
