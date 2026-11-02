# 第三方组件声明 / Third-Party Notices

szNote 本体（Go 后端、前端逻辑、样式）为原创代码，采用 GNU AGPL-3.0（另有商业许可，见
[COMMERCIAL-LICENSE.md](COMMERCIAL-LICENSE.md)）。
前端内嵌了以下第三方库（随二进制一起分发，未做修改）：

| 组件 | 版本 | 许可 | 用途 | 主页 |
|---|---|---|---|---|
| marked | 18.1.2 | MIT | Markdown → HTML 渲染 | https://github.com/markedjs/marked |
| DOMPurify | 3.4.17 | Apache-2.0 OR MPL-2.0 | 渲染结果 XSS 净化 | https://github.com/cure53/DOMPurify |

两者的完整许可文本分别见：
- marked: https://github.com/markedjs/marked/blob/master/LICENSE.md
- DOMPurify: https://github.com/cure53/DOMPurify/blob/main/LICENSE

Go 标准库随 Go 发行版分发，遵循 Go 的 BSD-3-Clause 许可。
