// iOS does not shrink the layout viewport when the software keyboard opens.
// It leaves the page its full height and pans the *visual* viewport to reveal
// the caret -- and a position: fixed element does not travel with that pan. The
// detail panel is fixed and inset: 0 on a phone and holds several editable
// fields, so its lower half sits behind the keyboard with no way to reach it.
//
// visualViewport is the only API that reports this. The difference between the
// layout height and the visible height is the keyboard, published as a CSS
// variable so the panel can shorten itself by exactly that much and let its own
// overflow scroll do the rest.
//
// Deliberately no transition on anything driven by --kb-inset: the keyboard
// animates itself, and a second animation on top of it makes the panel trail
// behind the keyboard edge.

export function trackKeyboardInset(): void {
  const vv = window.visualViewport
  if (!vv) return

  const root = document.documentElement

  const apply = () => {
    // offsetTop matters: when Safari has panned to reveal the caret, part of the
    // shortfall is above the visible area rather than below it, and only the
    // part below is keyboard.
    const inset = Math.max(0, window.innerHeight - vv.height - vv.offsetTop)
    root.style.setProperty('--kb-inset', `${Math.round(inset)}px`)
    // A threshold rather than inset > 0, because Safari's collapsing URL bar
    // produces a small persistent difference that is not a keyboard.
    root.classList.toggle('kb-open', inset > 80)
  }

  vv.addEventListener('resize', apply)
  vv.addEventListener('scroll', apply)
  apply()
}

// Shortening the panel is necessary but not sufficient: a field near the bottom
// can still land under the keyboard on the frame it gains focus, before the
// resize event has fired. Scrolling it into view afterwards costs nothing when
// it is already visible.
//
// Capture phase, because focus does not bubble.
export function scrollFocusedFieldIntoView(): void {
  document.addEventListener(
    'focusin',
    (e) => {
      const el = e.target as HTMLElement | null
      if (!el || !el.matches('input, textarea, select')) return
      if (!window.visualViewport) return
      // Wait for the keyboard to have resized the viewport, otherwise the
      // scroll is computed against a height that is about to change.
      window.setTimeout(() => {
        el.scrollIntoView({ block: 'center', behavior: 'smooth' })
      }, 250)
    },
    true,
  )
}
