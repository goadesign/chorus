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
				Background("#2563eb")
				Color("#ffffff")
				Stroke("#1d4ed8")
				FontSize(22)
			})
			ElementStyle("Server", func() {
				Shape(ShapeRoundedBox)
				Background("#0d9488")
				Color("#ffffff")
				Stroke("#0f766e")
				FontSize(22)
			})
			ElementStyle("ControlPlane", func() {
				Shape(ShapeCylinder)
				Background("#4f46e5")
				Color("#ffffff")
				Stroke("#3730a3")
				FontSize(22)
			})
			ElementStyle("DataPlane", func() {
				Shape(ShapePipe)
				Background("#0284c7")
				Color("#ffffff")
				Stroke("#0369a1")
				FontSize(22)
			})
			ElementStyle("Lock", func() {
				Shape(ShapeHexagon)
				Background("#d97706")
				Color("#ffffff")
				Stroke("#b45309")
				FontSize(22)
			})
			RelationshipStyle("Relationship", func() {
				Thickness(2)
				Color("#475569")
				FontSize(20)
			})
		})
	})
})
