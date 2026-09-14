# TUI Gopher

当前使用 `original-pixel.png`（用户确认的原图像素化稿），输入原图保存在 `original-reference.png`。图片转换使用内置 imagegen，提示词和原作者许可见 `original-pixel-prompt.md`。

`gopher.go` 中的两份字符网格直接由同一图像取样，没有重新设计五官或添加道具。大版 40×54 像素，占 40×27 终端字符；紧凑版 26×36 像素，占 26×18 字符。

在此目录重建网格（输出至标准输出后更新 `gopher.go`）：

```sh
go run rasterize.go original-pixel.png 40 54
go run rasterize.go original-pixel.png 26 36
```

转换器移除与边缘相连的黑底，以区域取样保留细线，再量化到固定色板。黑底映射为空终端单元；内部瞳孔、鼻子和眼眶不作为背景移除。运行时仅渲染 Unicode 半块字符，不解码 PNG，不依赖图片协议。

`concept-c.png`、`pixel-reference.png` 是之前的动作探索稿，不被 TUI 使用。
