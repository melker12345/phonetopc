package io.github.melker12345.phonetopc.panels

import io.github.melker12345.phonetopc.protocol.panels.Layout
import io.github.melker12345.phonetopc.protocol.panels.Widget
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

/** The sizing decisions of PANELS.md §5, including the two that once squeezed out a canvas. */
class PanelLayoutTest {
    private fun plan(json: String): LayoutNode = PanelLayout.plan(Layout.parse(Json.parseToJsonElement(json)))

    private fun LayoutNode.child(vararg path: Int): LayoutNode = path.fold(this) { n, i -> n.children[i] }

    private val canvas = """{"type":"canvas","id":"c"}"""
    private fun button(id: String, extra: String = "") = """{"type":"button","id":"$id","label":"$id"$extra}"""

    @Test fun rootFillsTheScreenAndIsSized() {
        val p = plan(button("b")).placement
        assertTrue(p.fillWidth && p.fillHeight && p.innerFillWidth && p.innerFillHeight && p.sized)
        assertNull(p.weight)
    }

    @Test fun canvasWithoutWeightInAColumnCountsAsWeightOne() {
        val root = plan("""{"type":"column","children":[$canvas,${button("b")}]}""")
        val c = root.child(0).placement
        assertEquals(1f, c.weight)
        assertTrue(c.sized)
        assertTrue(c.fillWidth)
        assertFalse(c.fillHeight) // the weight gives it its height
        // The button has its natural height, the full width.
        val b = root.child(1).placement
        assertNull(b.weight)
        assertFalse(b.sized)
        assertTrue(b.fillWidth && b.innerFillWidth)
        assertFalse(root.scrolls)
    }

    @Test fun explicitWeightWins() {
        val root = plan("""{"type":"column","children":[{"type":"canvas","id":"c","weight":3},${button("b", ""","weight":0.5""")}]}""")
        assertEquals(3f, root.child(0).placement.weight)
        assertEquals(0.5f, root.child(1).placement.weight)
    }

    @Test fun rowOfWeightedButtonsUnderACanvasIsNotSized() {
        // column(canvas, row(button weight=1)): the row has no height of its own, so the button
        // must not fill it, or the row takes all the column's height and the canvas gets 0 px.
        val root = plan("""{"type":"column","children":[$canvas,{"type":"row","children":[${button("b", ""","weight":1""")}]}]}""")
        val row = root.child(1)
        assertNull(row.placement.weight)
        assertFalse(row.placement.sized)
        val b = row.child(0).placement
        assertEquals(1f, b.weight)
        assertFalse(b.fillHeight)
        assertFalse(b.sized)
        assertTrue(b.innerFillWidth)
        assertFalse(b.innerFillHeight)
    }

    @Test fun weightedChildOfASizedRowFillsItsHeight() {
        val root = plan("""{"type":"row","children":[${button("a", ""","weight":1""")},${button("b")}]}""")
        val a = root.child(0).placement
        assertEquals(1f, a.weight)
        assertTrue(a.fillHeight && a.sized)
        assertTrue(a.innerFillWidth)
        assertFalse(a.innerFillHeight) // a button keeps its own height, centred
        val b = root.child(1).placement
        assertNull(b.weight)
        assertFalse(b.fillWidth || b.fillHeight || b.innerFillWidth || b.innerFillHeight || b.sized)
    }

    @Test fun gridOfButtonsUnderACanvasHasItsNaturalHeight() {
        val root = plan("""{"type":"column","children":[$canvas,{"type":"grid","cols":3,"children":[${button("a")},${button("b")}]}]}""")
        val grid = root.child(1)
        assertNull(grid.placement.weight) // not weight 1: only canvases get that by default
        assertFalse(grid.placement.sized)
        grid.children.forEach {
            val p = it.placement
            assertTrue(p.fillWidth && p.innerFillWidth)
            assertFalse(p.fillHeight || p.innerFillHeight || p.sized)
        }
    }

    @Test fun weightedGridHasSizedCells() {
        val root = plan("""{"type":"column","children":[$canvas,{"type":"grid","cols":2,"weight":1,"children":[${button("a")},${button("b")}]}]}""")
        val grid = root.child(1)
        assertEquals(1f, grid.placement.weight)
        assertTrue(grid.placement.sized)
        grid.children.forEach {
            val p = it.placement
            assertTrue(p.fillWidth && p.fillHeight && p.innerFillWidth && p.innerFillHeight && p.sized)
            assertNull(p.weight)
        }
    }

    @Test fun rootGridIsSized() {
        val grid = plan("""{"type":"grid","cols":2,"children":[${button("a")},${button("b")},${button("c")}]}""")
        assertTrue(grid.placement.sized)
        assertEquals(3, grid.children.size)
        assertTrue(grid.children.all { it.placement.sized && it.placement.innerFillHeight })
    }

    @Test fun gridHoldingACanvasFills() {
        val root = plan("""{"type":"column","children":[${button("b")},{"type":"grid","cols":1,"children":[$canvas]}]}""")
        assertTrue(PanelLayout.fills(root.child(1).widget))
        assertEquals(1f, root.child(1).placement.weight)
        assertTrue(root.child(1).placement.sized)
        assertTrue(root.child(1, 0).placement.sized)
        assertFalse(root.scrolls)
    }

    @Test fun unweightedColumnScrolls() {
        val root = plan("""{"type":"column","children":[${button("a")},{"type":"text","text":"hi"},${button("b")}]}""")
        assertTrue(root.scrolls)
        assertTrue(root.children.none { it.placement.sized })
    }

    @Test fun onlyColumnsScroll() {
        assertFalse(plan("""{"type":"row","children":[${button("a")}]}""").scrolls)
        assertFalse(plan("""{"type":"grid","cols":1,"children":[${button("a")}]}""").scrolls)
    }

    @Test fun nestedRowInAWeightedRow() {
        // The root row is sized; its weighted inner row fills that height and passes it on to its
        // own weighted children, but not to the unweighted ones.
        val root = plan(
            """{"type":"row","children":[
                {"type":"row","weight":2,"children":[${button("a", ""","weight":1""")},${button("b")}]},
                ${button("c")}
            ]}""",
        )
        val inner = root.child(0)
        assertEquals(2f, inner.placement.weight)
        assertTrue(inner.placement.sized && inner.placement.fillHeight)
        assertTrue(inner.child(0).placement.sized && inner.child(0).placement.fillHeight)
        assertFalse(inner.child(1).placement.sized || inner.child(1).placement.fillHeight)
    }

    @Test fun nestedRowInAnUnsizedRowStaysUnsized() {
        // In a scrolling column nothing is sized, however deep the weights go.
        val root = plan(
            """{"type":"column","children":[{"type":"row","children":[
                {"type":"row","weight":1,"children":[${button("a", ""","weight":1""")}]}
            ]}]}""",
        )
        assertTrue(root.scrolls)
        val inner = root.child(0, 0)
        assertEquals(1f, inner.placement.weight)
        assertFalse(inner.placement.sized || inner.placement.fillHeight)
        assertFalse(inner.child(0).placement.sized || inner.child(0).placement.fillHeight)
    }

    @Test fun canvasInARowTakesTheRowsHeight() {
        val root = plan("""{"type":"column","children":[{"type":"row","children":[$canvas,${button("b")}]}]}""")
        val row = root.child(0)
        // The row holds a canvas, so it fills: weight 1 in the column, which sizes it.
        assertEquals(1f, row.placement.weight)
        assertTrue(row.placement.sized)
        val c = row.child(0).placement
        assertEquals(1f, c.weight)
        assertTrue(c.fillHeight && c.sized)
        assertFalse(root.scrolls)
    }

    @Test fun weightOfOnlyDefaultsForFillingWidgets() {
        assertNull(PanelLayout.weightOf(Layout.parse(Json.parseToJsonElement(button("b")))))
        assertEquals(1f, PanelLayout.weightOf(Layout.parse(Json.parseToJsonElement(canvas))))
        val grid = Layout.parse(Json.parseToJsonElement("""{"type":"grid","cols":1,"children":[${button("b")}]}"""))
        assertTrue(grid is Widget.Grid)
        assertNull(PanelLayout.weightOf(grid))
    }
}
