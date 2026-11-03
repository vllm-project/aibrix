const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");
const vm = require("node:vm");

const switcherSource = fs.readFileSync(
  path.join(__dirname, "..", "source", "_static", "language-switcher.js"),
  "utf8"
);

function switchLanguage(location, target, hosted = false) {
  let changeHandler;
  let assignedUrl;
  const select = {
    dataset: { hosted: String(hosted) },
    options: [{ value: "en" }, { value: "zh-cn" }],
    value: "en",
    addEventListener(event, handler) {
      assert.equal(event, "change");
      changeHandler = handler;
    },
  };
  const browserLocation = {
    ...location,
    assign(url) {
      assignedUrl = url;
    },
  };

  vm.runInNewContext(switcherSource, {
    document: {
      readyState: "complete",
      querySelectorAll(selector) {
        assert.equal(selector, ".aibrix-lang-switcher select");
        return [select];
      },
    },
    window: { location: browserLocation },
  });

  select.value = target;
  changeHandler();
  return assignedUrl;
}

test("switches a hosted English page to the nested Chinese page", () => {
  const actual = switchLanguage(
    {
      protocol: "https:",
      host: "aibrix.readthedocs.io",
      origin: "https://aibrix.readthedocs.io",
      pathname: "/latest/features/runtime.html",
      search: "?view=full",
      hash: "#configuration",
    },
    "zh-cn",
    true
  );

  assert.equal(
    actual,
    "https://aibrix.readthedocs.io/latest/zh-cn/features/runtime.html?view=full#configuration"
  );
});

test("switches a nested Chinese page back to the hosted English page", () => {
  const actual = switchLanguage(
    {
      protocol: "https:",
      host: "aibrix.readthedocs.io",
      origin: "https://aibrix.readthedocs.io",
      pathname: "/latest/zh-cn/features/runtime.html",
      search: "",
      hash: "",
    },
    "en",
    true
  );

  assert.equal(
    actual,
    "https://aibrix.readthedocs.io/latest/features/runtime.html"
  );
});

test("uses the hosted layout on a Read the Docs custom domain", () => {
  const actual = switchLanguage(
    {
      protocol: "https:",
      host: "docs.aibrix.ai",
      origin: "https://docs.aibrix.ai",
      pathname: "/latest/getting_started/overview.html",
      search: "",
      hash: "",
    },
    "zh-cn",
    true
  );

  assert.equal(
    actual,
    "https://docs.aibrix.ai/latest/zh-cn/getting_started/overview.html"
  );
});

test("switches a file preview to the nested Chinese build", () => {
  const actual = switchLanguage(
    {
      protocol: "file:",
      host: "",
      origin: "null",
      pathname: "/workspace/aibrix/docs/build/html/index.html",
      search: "",
      hash: "#contents",
    },
    "zh-cn"
  );

  assert.equal(
    actual,
    "file:///workspace/aibrix/docs/build/html/zh-cn/index.html#contents"
  );
});

test("does not treat an unrelated zh-cn directory as the language marker", () => {
  const actual = switchLanguage(
    {
      protocol: "file:",
      host: "",
      origin: "null",
      pathname: "/workspace/zh-cn/aibrix/docs/build/html/index.html",
      search: "",
      hash: "",
    },
    "zh-cn"
  );

  assert.equal(
    actual,
    "file:///workspace/zh-cn/aibrix/docs/build/html/zh-cn/index.html"
  );
});
