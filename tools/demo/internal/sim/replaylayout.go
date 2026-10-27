package sim

// Screen model of the simulated client apps for session replay: every page / screen is a list of
// absolutely positioned boxes. The same model is rendered as an rrweb DOM (web) and as wireframe
// nodes (mobile), and gives click / touch targets exact coordinates.

import (
	"fmt"
	"math/rand/v2"
	"strings"
)

// box is one element of a screen. Coordinates are CSS px / dp, relative to the document (web) or the
// screen (mobile, before scrolling).
type box struct {
	kind       string // view | text | button | input | image | icon | list | switch
	x, y, w, h float64
	bg, fg     string
	border     string
	radius     float64
	text       string
	unmask     bool   // chrome label (data-trooper-unmask / trooperUnmask): recorded in clear
	role       string // "" | click | input | search | password
	fixed      bool   // mobile: not scrolled (status / nav / tab bar)
}

func (b box) cx() float64 { return b.x + b.w/2 }
func (b box) cy() float64 { return b.y + b.h/2 }

type theme struct{ primary, bg, surface, text, muted, skeleton, line string }

func themeOf(app string) theme {
	t := theme{primary: "#ff6000", bg: "#ffffff", surface: "#f5f5f5", text: "#1f1f1f", muted: "#8c8c8c", skeleton: "#e9e9ec", line: "#e5e5e5"}
	switch app {
	case "backoffice-web":
		t.primary = "#2f54eb"
	case "seller-portal":
		t.primary = "#13a8a8"
	case "landing-web":
		t.primary = "#722ed1"
	case "courier-app":
		t.primary = "#389e0d"
	}
	return t
}

var (
	productNames = []string{"Red Runner Sneaker", "Linen Overshirt", "Ceramic Table Lamp", "Black Bomber Jacket", "Stoneware Mug Set",
		"Chrono Field Watch", "Merino Crew Socks", "Canvas Weekender Bag", "Oak Desk Organizer", "Wireless Earbuds Pro"}
	brands = map[string]string{"shop-web": "shop", "shop-mobile": "shop", "backoffice-web": "Backoffice", "seller-portal": "Seller Portal",
		"landing-web": "shop", "courier-app": "Courier"}
)

func price() string {
	return fmt.Sprintf("₺%d,%02d", 49+rand.IntN(4800), []int{90, 99, 50, 0}[rand.IntN(4)])
}

// screenLayout builds the boxes of a page: chrome (header / bars) plus content. loading = skeleton
// placeholders while the view's data loads. Returns the boxes and the document height.
func screenLayout(a clientApp, p page, W, H float64, loading bool) (chrome, content []box, docH float64) {
	t := themeOf(a.name)
	mobile := a.mobile || W < 600
	pad := 24.0
	if mobile {
		pad = 16
	}
	cw := min(W-2*pad, 1160)
	x0 := (W - cw) / 2
	y := 0.0
	add := func(b box) { content = append(content, b) }
	txt := func(x, y, w, h float64, s string, fg string, unmask bool) box {
		return box{kind: "text", x: x, y: y, w: w, h: h, text: s, fg: fg, unmask: unmask}
	}
	sk := func(b box) box { // skeleton version of a box
		if loading {
			b.text, b.bg, b.border, b.role = "", t.skeleton, "", ""
			if b.kind == "text" {
				b.kind = "view"
			}
		}
		return b
	}

	// ---- chrome
	if a.mobile {
		chrome = append(chrome,
			box{kind: "view", x: 0, y: 0, w: W, h: 47, bg: t.bg, fixed: true},
			box{kind: "text", x: 24, y: 14, w: 48, h: 20, text: "9:41", fg: t.text, unmask: true, fixed: true},
			box{kind: "icon", x: W - 80, y: 16, w: 56, h: 14, bg: t.text, radius: 3, fixed: true},
			box{kind: "view", x: 0, y: 47, w: W, h: 44, bg: t.bg, border: t.line, fixed: true},
			box{kind: "text", x: 56, y: 57, w: W - 112, h: 24, text: screenTitle(p.name), fg: t.text, unmask: true, fixed: true},
			box{kind: "icon", x: 16, y: 59, w: 20, h: 20, bg: t.muted, radius: 4, role: "click", fixed: true},
		)
		tabs := []string{"Home", "Search", "Cart", "Account"}
		if a.name == "courier-app" {
			tabs = []string{"Deliveries", "Map", "Earnings", "Profile"}
		}
		tw := W / float64(len(tabs))
		chrome = append(chrome, box{kind: "view", x: 0, y: H - 83, w: W, h: 83, bg: t.bg, border: t.line, fixed: true})
		for i, name := range tabs {
			fg := t.muted
			if strings.EqualFold(name, screenTitle(p.name)) || i == 0 && strings.Contains(p.name, "Home") {
				fg = t.primary
			}
			chrome = append(chrome,
				box{kind: "icon", x: tw*float64(i) + tw/2 - 12, y: H - 75, w: 24, h: 24, bg: fg, radius: 6, role: "click", fixed: true},
				box{kind: "text", x: tw * float64(i), y: H - 48, w: tw, h: 14, text: name, fg: fg, unmask: true, fixed: true})
		}
		y = 91 + pad
	} else {
		chrome = append(chrome,
			box{kind: "view", x: 0, y: 0, w: W, h: 64, bg: t.bg, border: t.line},
			box{kind: "text", x: x0, y: 18, w: 140, h: 28, text: brands[a.name], fg: t.primary, unmask: true},
		)
		if a.name == "shop-web" || a.name == "landing-web" {
			if !mobile {
				chrome = append(chrome, box{kind: "input", x: x0 + 180, y: 14, w: min(420, cw-420), h: 36, bg: t.surface, radius: 18, role: "search", text: ""})
			}
			chrome = append(chrome,
				box{kind: "button", x: x0 + cw - 96, y: 14, w: 96, h: 36, bg: t.primary, fg: "#ffffff", radius: 18, text: "Cart (2)", unmask: true, role: "click"},
				box{kind: "text", x: x0 + cw - 190, y: 20, w: 80, h: 24, text: "Account", fg: t.text, unmask: true, role: "click"})
		} else {
			for i, n := range navOf(a) {
				chrome = append(chrome, box{kind: "text", x: x0 + 200 + float64(i)*110, y: 20, w: 100, h: 24, text: n, fg: t.text, unmask: true, role: "click"})
			}
		}
		y = 64 + pad
	}

	// ---- content
	name := strings.ToLower(p.name)
	heading := func(s string) {
		add(sk(box{kind: "text", x: x0, y: y, w: cw, h: 32, text: s, fg: t.text, unmask: true}))
		y += 44
	}
	grid := func(n, cols int, cardH float64, withButton bool) {
		gap := 16.0
		w := (cw - gap*float64(cols-1)) / float64(cols)
		for i := 0; i < n; i++ {
			cx := x0 + float64(i%cols)*(w+gap)
			cy := y + float64(i/cols)*(cardH+gap)
			add(sk(box{kind: "view", x: cx, y: cy, w: w, h: cardH, bg: t.bg, border: t.line, radius: 12}))
			add(sk(box{kind: "image", x: cx + 8, y: cy + 8, w: w - 16, h: cardH * 0.55, bg: t.surface, radius: 8}))
			add(sk(txt(cx+8, cy+cardH*0.55+16, w-16, 18, productNames[rand.IntN(len(productNames))], t.text, false)))
			add(sk(txt(cx+8, cy+cardH*0.55+38, w/2, 18, price(), t.text, false)))
			if withButton {
				add(sk(box{kind: "button", x: cx + w - 88, y: cy + cardH - 44, w: 80, h: 34, bg: t.primary, fg: "#ffffff", radius: 17, text: "Add", unmask: true, role: "click"}))
			}
		}
		rows := (n + cols - 1) / cols
		y += float64(rows)*(cardH+gap) + 8
	}
	rows := func(n int, rowH float64, cols []string, action string) {
		if !mobile {
			for i, c := range cols {
				add(sk(txt(x0+float64(i)*cw/float64(len(cols)), y, cw/float64(len(cols)), 20, c, t.muted, true)))
			}
			y += 32
		}
		for i := 0; i < n; i++ {
			add(sk(box{kind: "view", x: x0, y: y, w: cw, h: rowH, bg: t.bg, border: t.line, radius: 8, role: "click"}))
			if mobile {
				add(sk(txt(x0+12, y+10, cw*0.6, 18, fmt.Sprintf("#%d · %s", 180000+rand.IntN(9999), productNames[rand.IntN(len(productNames))]), t.text, false)))
				add(sk(txt(x0+12, y+rowH-28, cw*0.4, 16, price(), t.muted, false)))
			} else {
				for j := range cols {
					s := []string{fmt.Sprintf("#%d", 180000+rand.IntN(9999)), productNames[rand.IntN(len(productNames))], price(), "In transit", "Oct 2" + fmt.Sprint(rand.IntN(8))}[j%5]
					add(sk(txt(x0+float64(j)*cw/float64(len(cols))+12, y+(rowH-18)/2, cw/float64(len(cols))-24, 18, s, t.text, false)))
				}
			}
			if action != "" {
				add(sk(box{kind: "button", x: x0 + cw - 92, y: y + (rowH-30)/2, w: 80, h: 30, bg: t.surface, fg: t.text, radius: 15, text: action, unmask: true, role: "click"}))
			}
			y += rowH + 8
		}
		y += 8
	}
	form := func(fields []string, submit string) {
		for _, f := range fields {
			add(sk(txt(x0, y, cw, 18, f, t.muted, true)))
			role := "input"
			if strings.Contains(f, "Card") || strings.Contains(f, "CVC") || strings.Contains(f, "Password") {
				role = "password"
			}
			add(sk(box{kind: "input", x: x0, y: y + 22, w: min(cw, 520), h: 44, bg: t.bg, border: t.line, radius: 8, role: role}))
			y += 84
		}
		add(sk(box{kind: "button", x: x0, y: y, w: min(cw, 520), h: 48, bg: t.primary, fg: "#ffffff", radius: 24, text: submit, unmask: true, role: "click"}))
		y += 72
	}
	detail := func(buttons ...string) {
		imgW := cw
		if !mobile {
			imgW = cw * 0.5
		}
		add(sk(box{kind: "image", x: x0, y: y, w: imgW, h: min(imgW, 420), bg: t.surface, radius: 12}))
		dx, dy := x0, y+min(imgW, 420)+16
		if !mobile {
			dx, dy = x0+imgW+32, y
		}
		dw := cw - (dx - x0)
		add(sk(txt(dx, dy, dw, 30, productNames[rand.IntN(len(productNames))], t.text, false)))
		add(sk(txt(dx, dy+42, 160, 26, price(), t.primary, false)))
		for i := 0; i < 4; i++ {
			add(sk(txt(dx, dy+84+float64(i)*22, dw*(0.95-0.1*float64(i%2)), 18, "Soft knit upper, cushioned midsole and a grippy rubber outsole for everyday wear.", t.muted, false)))
		}
		for i, b := range buttons {
			bg, fg := t.primary, "#ffffff"
			if i > 0 {
				bg, fg = t.surface, t.text
			}
			add(sk(box{kind: "button", x: dx, y: dy + 190 + float64(i)*58, w: min(dw, 320), h: 48, bg: bg, fg: fg, radius: 24, text: b, unmask: true, role: "click"}))
		}
		y = max(y+min(imgW, 420), dy+190+float64(len(buttons))*58) + 24
	}
	steps := func() {
		for i, s := range []string{"Order placed", "Preparing", "Shipped", "Out for delivery", "Delivered"} {
			bg := t.skeleton
			if i <= 3 {
				bg = t.primary
			}
			add(sk(box{kind: "icon", x: x0, y: y + 4, w: 16, h: 16, bg: bg, radius: 8}))
			add(sk(txt(x0+28, y, cw-28, 22, s, t.text, true)))
			y += 40
		}
		y += 12
	}

	switch {
	case name == "/" && a.name == "landing-web", strings.Contains(name, "campaign"):
		add(sk(box{kind: "image", x: 0, y: y - pad, w: W, h: 420, bg: t.primary}))
		add(sk(txt(x0, y+80, cw*0.6, 48, "Autumn sale — up to 40% off", "#ffffff", true)))
		add(sk(box{kind: "button", x: x0, y: y + 160, w: 180, h: 52, bg: "#ffffff", fg: t.primary, radius: 26, text: "Shop now", unmask: true, role: "click"}))
		y += 420
		grid(4, cols(mobile, 4), 300, false)
	case name == "/" || name == "home" || name == "/dashboard":
		if a.name == "seller-portal" {
			heading("Dashboard")
			for i, k := range []string{"Revenue", "Orders", "Returns", "Rating"} {
				w := (cw - 48) / 4
				if mobile {
					w = cw
				}
				bx := x0 + float64(i)*(w+16)
				by := y
				if mobile {
					bx, by = x0, y+float64(i)*104
				}
				add(sk(box{kind: "view", x: bx, y: by, w: w, h: 88, bg: t.bg, border: t.line, radius: 12}))
				add(sk(txt(bx+16, by+12, w-32, 18, k, t.muted, true)))
				add(sk(txt(bx+16, by+40, w-32, 30, price(), t.text, false)))
			}
			y += 120
			if mobile {
				y += 312
			}
			add(sk(box{kind: "view", x: x0, y: y, w: cw, h: 280, bg: t.surface, radius: 12}))
			y += 300
			rows(5, 52, []string{"Order", "Product", "Total", "Status", "Date"}, "")
		} else {
			add(sk(box{kind: "image", x: x0, y: y, w: cw, h: map[bool]float64{true: 180, false: 320}[mobile], bg: t.primary, radius: 16}))
			y += map[bool]float64{true: 196, false: 340}[mobile]
			heading("For you")
			grid(8, cols(mobile, 4), 300, true)
			heading("Trending now")
			grid(4, cols(mobile, 4), 300, true)
		}
	case strings.Contains(name, "edit"):
		heading("Edit product")
		form([]string{"Title", "Price", "Stock", "Description"}, "Save changes")
	case strings.Contains(name, "slug") || name == "productdetail":
		detail("Add to cart", "Add to wishlist")
		heading("Similar items")
		grid(4, cols(mobile, 4), 280, true)
	case strings.Contains(name, "search"):
		heading("Search")
		add(sk(box{kind: "input", x: x0, y: y, w: min(cw, 640), h: 44, bg: t.surface, radius: 22, role: "search"}))
		y += 60
		for i, c := range []string{"Shoes", "Under ₺500", "Free shipping", "Rating 4+"} {
			add(sk(box{kind: "button", x: x0 + float64(i)*124, y: y, w: 116, h: 32, bg: t.surface, fg: t.text, radius: 16, text: c, unmask: true, role: "click"}))
		}
		y += 52
		grid(8, cols(mobile, 4), 280, true)
	case name == "/cart" || name == "cart":
		heading("Your cart")
		rows(3, 96, nil, "Remove")
		add(sk(txt(x0, y, cw/2, 24, "Total", t.text, true)))
		add(sk(txt(x0+cw-160, y, 160, 24, price(), t.text, false)))
		y += 40
		add(sk(box{kind: "button", x: x0, y: y, w: min(cw, 520), h: 52, bg: t.primary, fg: "#ffffff", radius: 26, text: "Go to checkout", unmask: true, role: "click"}))
		y += 76
	case strings.Contains(name, "checkout"):
		heading("Checkout")
		form([]string{"Full name", "Address", "City", "Card number", "Expiry", "CVC"}, "Pay now")
	case strings.Contains(name, "orders/:id") || name == "ordertracking" || name == "deliverydetail":
		heading("Order details")
		steps()
		if a.name == "courier-app" {
			add(sk(box{kind: "view", x: x0, y: y, w: cw, h: 220, bg: "#dfe9d8", radius: 12}))
			y += 236
			add(sk(box{kind: "button", x: x0, y: y, w: cw, h: 52, bg: t.primary, fg: "#ffffff", radius: 26, text: "Navigate", unmask: true, role: "click"}))
			y += 64
			add(sk(box{kind: "button", x: x0, y: y, w: cw, h: 52, bg: t.surface, fg: t.text, radius: 26, text: "Call customer", unmask: true, role: "click"}))
			y += 76
		} else {
			rows(2, 88, nil, "")
			add(sk(box{kind: "button", x: x0, y: y, w: min(cw, 320), h: 48, bg: t.primary, fg: "#ffffff", radius: 24, text: "Chat with support", unmask: true, role: "click"}))
			add(sk(box{kind: "button", x: x0 + min(cw, 320) + 16, y: y, w: min(cw, 200), h: 48, bg: t.surface, fg: t.text, radius: 24, text: "Cancel order", unmask: true, role: "click"}))
			if mobile {
				add(sk(box{kind: "button", x: x0, y: y + 60, w: cw, h: 48, bg: t.surface, fg: t.text, radius: 24, text: "Cancel order", unmask: true, role: "click"}))
				y += 60
			}
			y += 72
		}
	case strings.Contains(name, "proof"):
		heading("Proof of delivery")
		add(sk(box{kind: "view", x: x0, y: y, w: cw, h: 260, bg: "#1f1f1f", radius: 12}))
		y += 276
		add(sk(box{kind: "view", x: x0, y: y, w: cw, h: 140, bg: t.surface, border: t.line, radius: 12}))
		y += 156
		form([]string{"Recipient name"}, "Confirm delivery")
	case strings.Contains(name, "account") || strings.Contains(name, "customers"):
		heading("Account")
		add(sk(box{kind: "image", x: x0, y: y, w: 72, h: 72, bg: t.surface, radius: 36}))
		add(sk(txt(x0+88, y+10, 260, 24, "Customer name", t.text, false)))
		add(sk(txt(x0+88, y+40, 260, 18, "customer@example.com", t.muted, false)))
		y += 100
		heading("Recent orders")
		rows(4, 64, []string{"Order", "Product", "Total", "Status", "Date"}, "View")
	case strings.Contains(name, "reports"):
		heading("Reports")
		for i := 0; i < 4; i++ {
			add(sk(box{kind: "view", x: x0 + float64(i%2)*(cw/2+8), y: y + float64(i/2)*276, w: cw/2 - 8, h: 260, bg: t.surface, radius: 12}))
		}
		y += 560
	case strings.Contains(name, "pricing"):
		heading("Plans")
		w := (cw - 32) / 3
		for i, pl := range []string{"Basic", "Plus", "Premium"} {
			bx := x0 + float64(i)*(w+16)
			add(sk(box{kind: "view", x: bx, y: y, w: w, h: 360, bg: t.bg, border: t.line, radius: 16}))
			add(sk(txt(bx+20, y+20, w-40, 28, pl, t.text, true)))
			add(sk(txt(bx+20, y+60, w-40, 36, price(), t.primary, false)))
			add(sk(box{kind: "button", x: bx + 20, y: y + 290, w: w - 40, h: 48, bg: t.primary, fg: "#ffffff", radius: 24, text: "Choose", unmask: true, role: "click"}))
		}
		y += 384
	case strings.Contains(name, "blog"):
		add(sk(box{kind: "image", x: x0, y: y, w: cw, h: 300, bg: t.surface, radius: 12}))
		y += 320
		heading("How we pick sustainable materials")
		for i := 0; i < 14; i++ {
			add(sk(txt(x0, y, cw*(0.9+0.1*float64(i%2)), 20, strings.Repeat("Lorem ipsum dolor sit amet ", 4), t.text, false)))
			y += 28
		}
	case strings.Contains(name, "earnings"):
		heading("Earnings")
		add(sk(box{kind: "view", x: x0, y: y, w: cw, h: 120, bg: t.primary, radius: 12}))
		y += 136
		rows(6, 56, nil, "")
	default: // lists: /orders, /refunds, /payouts, /products, Deliveries
		heading(screenTitle(p.name))
		if strings.Contains(name, "products") {
			grid(8, cols(mobile, 4), 260, false)
		} else {
			action := "Open"
			if a.name == "courier-app" {
				action = "Start"
			}
			rows(8, 56, []string{"#", "Item", "Amount", "Status", "Date"}, action)
		}
	}
	docH = max(y+pad, H)
	if a.mobile {
		docH = max(y+83+pad, H)
	}
	return chrome, content, docH
}

func cols(mobile bool, n int) int {
	if mobile {
		return 2
	}
	return n
}

func navOf(a clientApp) []string {
	var out []string
	for _, p := range a.pages {
		if !strings.Contains(p.name, ":") {
			out = append(out, screenTitle(p.name))
		}
	}
	return out
}

// screenTitle turns "/account/orders" or "OrderTracking" into a readable title.
func screenTitle(name string) string {
	if name == "/" {
		return "Home"
	}
	s := strings.Trim(name, "/")
	if i := strings.LastIndexByte(s, '/'); i >= 0 && !strings.HasPrefix(s[i+1:], ":") {
		s = s[i+1:]
	} else if i >= 0 {
		s = s[:i]
		if j := strings.LastIndexByte(s, '/'); j >= 0 {
			s = s[j+1:]
		}
	}
	if s == "" {
		return name
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// maskText keeps the shape of masked text: rrweb replaces characters with '*', the mobile SDKs with '•'.
func maskText(s string, r rune) string {
	var b strings.Builder
	for _, c := range s {
		if c == ' ' {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
