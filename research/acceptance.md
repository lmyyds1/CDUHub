# CDU directory acceptance notes

Observed from the saved `zzjg.html` and `yxsz.html` snapshots on 2026-09-14. These are source-site facts, not fixed future limits. Directory extraction must stay inside `.n_container.znbm .content .box .list23` and ignore the duplicated desktop/mobile navigation and footer links.

| Directory | Section | Named links | HTTP(S) links |
| --- | --- | ---: | ---: |
| 组织机构 | 党群部门 | 13 | 13 |
| 组织机构 | 行政部门 | 18 | 18 |
| 组织机构 | 直属单位 | 6 | 5 |
| 组织机构 | 附属单位 | 6 | 6 |
| **组织机构合计** | | **43** | **42** |
| 院系设置 | 教学科研类（两个区块） | 25 | 24 |
| 院系设置 | 综合拔尖类 | 2 | 2 |
| 院系设置 | 中外合作类 | 2 | 2 |
| 院系设置 | 终身教育类 | 1 | 1 |
| **院系设置合计** | | **30** | **29** |

The full directory therefore has 73 named memberships, of which 71 have HTTP(S) destinations. These point to 67 distinct hosts. Counts of logical units, directory memberships, URLs and hosts are different; show clearly which one a statistic represents.

## Cases that must not silently disappear

- 体育场馆中心 and 化学化工学院 use `href="#"`. Keep them as unavailable sources with an explanatory status; do not crawl the directory page as their website.
- 党委保卫部 / 保卫处 and 科技处 / 高等研究院 use invalid nested anchor markup inside one list item. They expose two distinct websites each (`wzb` + `bwc`, `kjc` + `gdyjy`), and both must be found after HTML parser repair. The original label fragments have unbalanced punctuation.
- 医学中心 has three nested `.a1` links: 基础医学院, 临床医学院, 护理学院. 中国-东盟艺术学院 has four: 美术与设计学院, 影视与动画学院, 音乐与舞蹈学院, 国际部. Selecting only direct list-item anchors loses seven requested subunits.
- 基础医学院 and 护理学院 both link to `https://yhxy.cdu.edu.cn/`; preserve both labels or explicit aliases. This is 29 valid college memberships but 28 unique college websites.
- 医学中心 appears in both directories. 药学院 and 成都大学四川抗菌素工业研究所 share `pharm.cdu.edu.cn`. 中国-东盟艺术学院办公室 uses HTTP while 中国-东盟艺术学院 uses HTTPS on the same host. Preserve department/college memberships even when requests are deduplicated.
- Some college URLs omit the trailing slash, while most include it. Relative article and column URLs must resolve against the actual fetched page URL.
- The college page includes empty layout anchors and repeated section headings. Empty anchors are not sources; both 教学科研类 blocks are valid.

## End-to-end acceptance

1. Both official directory entry points are used; all 73 named memberships are represented, including unavailable entries and aliases.
2. Each reachable site is inspected for relevant columns (not just homepage articles). The user's example requires 领导动态, 通知通告, 业务流程, 党务工作 and 校务工作. Equivalent news, announcements, teaching, research, admissions and employment columns on other sites should be discovered from site links.
3. Every collected item retains its original title, original article URL, source membership, column and displayed publication date. A month/day date such as `09-03` must remain recognizably partial unless the detail page supplies its year; the current year must not be silently invented.
4. Relative `.htm` links, nested column paths, More/更多 links, next-page links and duplicate articles are handled; unrelated login, mail, resource download and off-site navigation links are excluded from column crawling.
5. Crawl limits and unavailable sites are visible. One unreachable site cannot abort results from other sites. User refresh does not create overlapping full crawls. Cancellation leaves already collected data consistent.
6. Persisted results survive restart. Search and source/type/column/date filters operate on stored articles. Links return readers to the public original page.

## Fixtures

`internal/crawler/testdata/zzjg-directory.html` and `yxsz-directory.html` retain every directory list and their malformed/nested/placeholder cases, remove unrelated scripts and assets, and add representative irrelevant header/footer links. They should parse to the counts above without including the header/footer traps. Full snapshots remain available under `research/` for manual verification.
