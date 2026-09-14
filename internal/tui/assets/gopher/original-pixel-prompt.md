# 原图像素化稿

- 输入：`original-reference.png`，用户提供的正面站立 Gopher。
- 输出：`original-pixel.png`。
- 模式：内置 imagegen，图片编辑（style-transfer），未使用 CLI。
- 已接入 TUI；不再沿用先前 C 的动作或道具。终端采用同源的两档尺寸，详见 `README.md`。
- 原 Go Gopher 由 Renée French 设计：https://go.dev/blog/gopher ，CC BY 4.0：https://creativecommons.org/licenses/by/4.0/ 。此图为像素化改编。

## 转换提示词

Use case: style-transfer.
Input image 1 is the EDIT TARGET, not loose inspiration.
Primary request: directly pixelate this exact supplied Go Gopher image. Treat this as a faithful raster-to-pixel conversion, NOT a redesign or a new drawing.
Keep the exact original front-facing upright pose, tall cyan capsule silhouette, width-to-height ratio, round ears, two enormous white oval eyes, pupil positions and white glints, black oval nose, beige double-lobed muzzle, two separately outlined white buck teeth, tiny sideways tan paws, and the two tan feet. Keep every feature in its original relative position and size. The user rejected prior redesigned sprites. It must remain unmistakably THIS SAME IMAGE.
Only change smooth curves into deliberate crisp stair-stepped square-pixel contours. Use a moderately fine logical pixel grid approximately 64 pixels wide and 88 pixels high, upscaled crisply with nearest-neighbor edges, so the eyes and muzzle do not collapse into crude blocks. Keep the original flat cyan, white, tan, and black colors; no new shading, highlights, blush, outlines in other colors, or gradients. Preserve the original background. Single full-body sprite, same portrait composition, all limbs visible, no cropping.
No new pose or expression, no tilt, no code card, no checkmark, no computer, no clothing, no props, no text, no decorative effects, no pixel grid overlay. Faithful pixelization of the supplied image only.
