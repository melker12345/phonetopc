package io.github.melker12345.phonetopc.ui

import java.util.Locale

/**
 * [v] for display: whole numbers without a fraction (`40`, not `40.0`), others with [decimals]
 * places. Always a decimal point (Locale.ROOT), the way the PC writes numbers, not the phone's
 * locale's comma.
 */
fun formatNumber(v: Double, decimals: Int): String =
    if (v == Math.rint(v) && kotlin.math.abs(v) < 1e15) v.toLong().toString() else "%.${decimals}f".format(Locale.ROOT, v)
