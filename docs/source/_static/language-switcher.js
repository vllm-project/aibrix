(function () {
  function rewritePath(path, target, hosted) {
    var localEnglish = "/build/html/";
    var localChinese = "/build/html/zh-cn/";
    var hostedChinese = /^\/([^/]+)\/zh-cn(?=\/|$)/;
    var rootChinese = /^\/zh-cn(?=\/|$)/;

    if (target === "en") {
      if (path.indexOf(localChinese) !== -1) {
        return path.replace(localChinese, localEnglish);
      }
      if (hosted) {
        return path.replace(hostedChinese, "/$1");
      }
      return path.replace(rootChinese, "");
    }
    if (path.indexOf(localEnglish) !== -1) {
      return path.replace(localEnglish, localChinese);
    }
    if (hosted) {
      if (hostedChinese.test(path)) {
        return path;
      }
      return path.replace(/^\/([^/]+)(\/|$)/, "/$1/zh-cn$2");
    }
    if (rootChinese.test(path)) {
      return path;
    }
    return path.replace(/^\//, "/zh-cn/");
  }

  function switchUrl(loc, path) {
    // file:// pages report origin as the string "null"; preserve the scheme.
    if (loc.protocol === "file:") {
      return "file://" + path + loc.search + loc.hash;
    }
    return loc.origin + path + loc.search + loc.hash;
  }

  function init() {
    var selects = document.querySelectorAll(".aibrix-lang-switcher select");
    if (!selects.length) {
      return;
    }

    Array.prototype.forEach.call(selects, function (select) {
      select.addEventListener("change", function () {
        var loc = window.location;
        loc.assign(
          switchUrl(
            loc,
            rewritePath(
              loc.pathname,
              select.value,
              select.dataset.hosted === "true"
            )
          )
        );
      });
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
