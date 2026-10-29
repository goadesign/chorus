// Package diagram declares the rendered C4 views and visual styles for the
// Chorus architecture model.
package diagram

import (
	_ "github.com/goadesign/chorus/diagram/model"
	. "goa.design/model/dsl"
)

var _ = Design("Chorus Architecture", "Views and visual styles for the Chorus multi-agent coordination architecture.", func() {
	Views(func() {
		ContainerView("Chorus", "Chorus Architecture", "How LLM coding agents coordinate through the Chorus Goa-AI MCP server and Pulse primitives in Redis.", func() {
			Title("Chorus Multi-Agent Coordination Architecture")
			AddDefault()
			SystemBoundariesVisible()
			AutoLayout(RankTopBottom, func() {
				RankSeparation(180)
				NodeSeparation(120)
			})
		})

		Styles(func() {
			ElementStyle("Agent", func() {
				Shape(ShapeRobot)
				Background("#1e293b")
				Color("#f8fafc")
				Stroke("#94a3b8")
				FontSize(22)
			})
			ElementStyle("Container", func() {
				Shape(ShapeRoundedBox)
				Background("#0f293a")
				Color("#f8fafc")
				Stroke("#2dd4bf")
				FontSize(22)
			})
			ElementStyle("Database", func() {
				Shape(ShapeCylinder)
				Background("#1e293b")
				Color("#f8fafc")
				Stroke("#38bdf8")
				FontSize(22)
			})
			ElementStyle("Event Queue", func() {
				Shape(ShapePipe)
				Background("#1e293b")
				Color("#f8fafc")
				Stroke("#38bdf8")
				FontSize(22)
			})
			ElementStyle("Lock", func() {
				Shape(ShapeHexagon)
				Background("#1e293b")
				Color("#f8fafc")
				Stroke("#38bdf8")
				FontSize(22)
			})
			RelationshipStyle("Relationship", func() {
				Thickness(2)
				Color("#8b949e")
				FontSize(20)
			})
		})
	})
})
