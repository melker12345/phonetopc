package io.github.melker12345.phonetopc.protocol

/**
 * Characters APPROVAL.md §4 and PANELS.md §8 forbid in display text besides Cc: bidirectional
 * controls, which reorder what is shown, and invisible characters and line separators, which
 * hide or move parts of it. A fixed list rather than a Unicode category, so every
 * implementation agrees whatever its Unicode version. ZWNJ and ZWJ (U+200C, U+200D) stay
 * allowed: scripts and emoji need them.
 */
fun isHidden(cp: Int): Boolean =
    // bidi
    cp == 0x061C || cp == 0x200E || cp == 0x200F || cp in 0x202A..0x202E || cp in 0x2066..0x2069 ||
        // invisible
        cp == 0x00AD || cp == 0x180E || cp == 0x200B || cp in 0x2060..0x2064 || cp in 0x206A..0x206F ||
        cp == 0xFEFF || cp in 0xFFF9..0xFFFB || cp in 0xE0000..0xE007F ||
        // line and paragraph separators
        cp == 0x2028 || cp == 0x2029

/** A PC or phone name (SECURITY.md §4): 1–100 code points, no Cc, nothing [isHidden]. */
fun isName(s: String): Boolean {
    val count = s.codePointCount(0, s.length)
    if (count < 1 || count > 100 || s.isBlank()) return false
    var i = 0
    while (i < s.length) {
        val cp = s.codePointAt(i)
        if (Character.getType(cp) == Character.CONTROL.toInt() || Character.getType(cp) == Character.SURROGATE.toInt() || isHidden(cp)) return false
        i += Character.charCount(cp)
    }
    return true
}
