(function () {
  function englishSegment(path) {
    if (/readthedocs/.test(window.location.host) || /\/en\//.test(path)) {
      return "en";
    }
    return "html";
  }

  function rewritePath(path, slugs, target) {
    var dest = target === "en" ? englishSegment(path) : target;
    var re = new RegExp("/(" + slugs.join("|") + ")/");
    if (re.test(path)) {
      return path.replace(re, "/" + dest + "/");
    }
    if (dest !== "en" && dest !== "html") {
      return path.replace(/^\//, "/" + dest + "/");
    }
    return path;
  }

  function init() {
    var selects = document.querySelectorAll(".aibrix-lang-switcher select");
    if (!selects.length) {
      return;
    }

    var slugs = Array.prototype.map.call(selects[0].options, function (option) {
      return option.value;
    });
    slugs.push("html");

    Array.prototype.forEach.call(selects, function (select) {
      select.addEventListener("change", function () {
        var loc = window.location;
        loc.assign(
          loc.origin + rewritePath(loc.pathname, slugs, select.value) + loc.search + loc.hash
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
