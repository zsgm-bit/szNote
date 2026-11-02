# szNote 商业许可（Commercial License）

Copyright (c) 2026 szNote authors — https://github.com/zsgm-bit/szNote

szNote 采用 **双轨授权（dual licensing）**：开源社区版按 **GNU AGPL-3.0** 授权（全文见 [LICENSE](LICENSE)），
无法满足 AGPL 义务的商业场景可另行购买 **商业许可**。

---

## 一、什么时候免费：AGPL-3.0

以下情形可直接在 AGPL-3.0 下免费使用，**无需付费、无需申请**：

- 个人学习、自用、兴趣爱好项目
- 教学、学术研究、非营利组织内部使用
- 公司内部做功能评估、POC、演示（不对外提供服务、不进入商业交付）
- 任何愿意完整履行 AGPL-3.0 义务的使用方式

**AGPL-3.0 的核心义务**（务必了解，这是选择商业许可的根本原因）：

1. **源码开放**：分发本软件或其衍生作品时，必须以 AGPL-3.0 提供完整对应源码。
2. **网络服务条款（§13）**：**只要让用户通过网络与修改后的版本交互**，就必须向这些用户提供获取完整
   对应源码的方式——包括你自己的修改。这一条覆盖了 SaaS，不要求"分发"这个动作。
3. **传染性**：与本软件结合的衍生作品整体须按 AGPL-3.0 授权。
4. **保留声明**：不得移除版权与许可声明。

> 换句话说：**把 szNote 部署成对内/对外的网页服务并做了修改，却不打算公开修改后的源码，就需要商业许可。**

## 二、什么时候需要购买：商业许可

出现下列任一情形，须取得作者的书面商业许可：

| 场景 | 说明 |
|---|---|
| 闭源内部部署并修改 | 在营利组织内部部署、修改源码，但不希望公开修改内容 |
| SaaS / 托管服务 | 以网页服务形式对外提供（含免费对外服务），且不公开修改后的源码 |
| 集成进商业产品 | 嵌入商业软件、硬件、一体机、行业解决方案并对外销售 |
| 二次开发 / 定制交付 | 为客户做定制开发、功能扩展并交付，且不开放源码 |
| OEM / 白标 | 去掉品牌标识后作为自有产品发布 |
| 收费服务 | 基于本项目的收费运维、支持、培训、咨询 |

商业许可授予的是**在 AGPL-3.0 之外的额外权利**，典型包括：

- 免于 AGPL-3.0 §13 的源码提供义务，可闭源使用与修改
- 可将本软件与自有闭源代码结合，不受 AGPL 传染性约束
- 可进行 OEM / 白标发布（需在合同中约定）
- 约定的技术支持、缺陷修复与升级期限

**不包含**：商标使用权（szNote 名称与标识）、作者署名权的放弃、对第三方法律责任的承担。

## 三、授权形式与联系

- **年度订阅**：按部署实例数或用户规模计价，含期间内升级
- **一次性买断**：指定版本永久使用，含 12 个月升级
- **OEM / 白标**：按产品线单独议价

获取报价与合同文本，请通过以下方式联系，并说明：使用主体、部署方式（内部 / 对外服务）、
是否需要修改源码、规模（实例数 / 用户数）：

- GitHub Issues：https://github.com/zsgm-bit/szNote/issues （标题注明 `[商业授权]`）
- 仓库主页联系方式为准

## 四、贡献与版权

为保证双轨授权在法律上成立，作者须持有本项目全部版权。向本仓库提交 Pull Request 即表示你同意：

1. 你拥有所提交内容的版权，或已获得权利人授权；
2. 你授予作者一项永久、全球、免费、可再许可的许可，允许作者以 AGPL-3.0 **及商业许可**两种方式
   使用、修改、分发你的贡献；
3. 你的贡献按 AGPL-3.0 授权给本项目其他使用者。

不同意上述条款时，请勿提交 Pull Request，可改为提交 Issue 描述需求。

## 五、免责声明

本软件按"现状"提供，不附带任何明示或暗示的担保，包括但不限于适销性、特定用途适用性与
不侵权的担保。无论基于合同、侵权或其他事由，作者均不对因本软件或本软件的使用而产生的
任何索赔、损害或其他责任承担责任。商业许可合同中的责任条款以合同文本为准。

---

# Commercial License (English Summary)

szNote is dual-licensed: **GNU AGPL-3.0** for open-source use, and a **commercial license** for
organizations that cannot comply with AGPL-3.0 — most commonly because AGPL §13 requires offering the
complete corresponding source (including your modifications) to every user interacting with the
software over a network.

A commercial license is required for, among others: closed-source internal deployment with
modifications, SaaS/hosted offerings without publishing source, embedding into commercial products or
hardware, paid custom development without source disclosure, OEM/white-labeling, and paid
support/training services built on this project.

The commercial license grants rights **additional to** AGPL-3.0, typically including: no §13 source
offering obligation, freedom to combine with proprietary code, OEM/white-label rights (per contract),
and agreed support/upgrade terms. It does **not** grant trademark rights to the szNote name or logo.

Available as annual subscription, perpetual buyout (with 12 months of upgrades), or OEM agreement.
Contact via https://github.com/zsgm-bit/szNote/issues with the `[商业授权]` / `[Commercial License]`
prefix, stating your entity, deployment model, whether you modify the source, and scale.

By submitting a Pull Request you agree to grant the author a perpetual, worldwide, royalty-free,
sublicensable license to use your contribution under both AGPL-3.0 and the commercial license;
without such a grant dual licensing is not legally possible, so please open an Issue instead if you
do not agree.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED. IN NO EVENT SHALL
THE AUTHORS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY ARISING FROM THE SOFTWARE.
