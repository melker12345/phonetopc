package io.github.melker12345.phonetopc.panels

import io.github.melker12345.phonetopc.protocol.panels.Widget

/**
 * How a widget sits in its parent (PANELS.md §5), decided apart from Compose so it can be tested
 * on the JVM. [PanelContent] turns it into modifiers: the widget's place is `weight` (in a row or
 * column) then the fills, and a leaf widget sits inside that place with the inner fills.
 *
 * @property weight share of the parent row's width or column's height; `null` for natural size.
 *   Grid cells are always equal and ignore it.
 * @property fillWidth the place fills the width the parent offers.
 * @property fillHeight the place fills the height the parent offers.
 * @property innerFillWidth a leaf fills its place's width.
 * @property innerFillHeight a leaf fills its place's height.
 * @property sized the parent gave the widget its height (the root, a grid cell of a sized grid, a
 *   weighted child of a column, or a weighted child of a sized row) rather than asking for its
 *   natural height. A sized grid shares that height between its rows.
 */
data class Placement(
    val weight: Float?,
    val fillWidth: Boolean,
    val fillHeight: Boolean,
    val innerFillWidth: Boolean,
    val innerFillHeight: Boolean,
    val sized: Boolean,
)

/**
 * A widget with its [placement] and its children's. [scrolls]: a column with no weighted child
 * has natural heights and may be taller than the screen, so it scrolls.
 */
class LayoutNode(val widget: Widget, val placement: Placement, val children: List<LayoutNode>) {
    val scrolls: Boolean get() = widget is Widget.Column && children.none { it.placement.weight != null }
}

object PanelLayout {
    /** The root fills the screen below the frame (§5). */
    private val ROOT = Placement(null, fillWidth = true, fillHeight = true, innerFillWidth = true, innerFillHeight = true, sized = true)

    fun plan(root: Widget): LayoutNode = node(root, ROOT)

    /** Whether a widget wants all the space it can get: a canvas, or a container holding one. */
    fun fills(w: Widget): Boolean = when (w) {
        is Widget.Canvas -> true
        is Widget.Column -> w.children.any(::fills)
        is Widget.Row -> w.children.any(::fills)
        is Widget.Grid -> w.children.any(::fills)
        else -> false
    }

    /**
     * §5: `weight`, or 1 for a widget that fills (a canvas without a weight counts as 1). Only
     * filling counts: a grid of buttons without a weight takes its natural height, it doesn't
     * share the column's height with a canvas.
     */
    fun weightOf(w: Widget): Float? = w.weight?.toFloat() ?: if (fills(w)) 1f else null

    private fun node(w: Widget, placement: Placement): LayoutNode {
        val sized = placement.sized
        val children = when (w) {
            is Widget.Column -> w.children.map { child ->
                val weight = weightOf(child)
                // Full width; a weight is the child's height, otherwise it has its natural one.
                node(child, Placement(weight, fillWidth = true, fillHeight = false, innerFillWidth = true, innerFillHeight = false, sized = weight != null))
            }
            is Widget.Row -> w.children.map { child ->
                val weight = weightOf(child)
                // Filling the height of a row that has none of its own would make the row take
                // all the height a column offers it, leaving nothing for the column's weighted
                // children, a canvas for example.
                val fillHeight = sized && weight != null
                node(child, Placement(weight, fillWidth = false, fillHeight = fillHeight, innerFillWidth = weight != null, innerFillHeight = false, sized = fillHeight))
            }
            // Cells are all the same width. Given a height (the root, a weight), the rows share
            // it and widgets fill their cell; otherwise rows are as tall as their tallest widget,
            // and widgets fill only the cell's width, so a row of buttons stays a row of buttons.
            is Widget.Grid -> w.children.map { child ->
                node(child, Placement(null, fillWidth = true, fillHeight = sized, innerFillWidth = true, innerFillHeight = sized, sized = sized))
            }
            else -> emptyList()
        }
        return LayoutNode(w, placement, children)
    }
}
