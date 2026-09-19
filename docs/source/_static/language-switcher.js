(function () {
  function englishSegment(path) {
    if (/readthedocs/.test(window.location.host) || /\/en\//.test(path)) {
      return "en";
    }
    return "html";
  }

  // Prefer RTD language prefixes and Sphinx local build dirs so we do not
  // rewrite unrelated path segments that happen to contain "html" or "en".
  function rewritePath(path, slugs, target) {
    var dest = target === "en" ? englishSegment(path) : target;
    var rtdMatch = path.match(/^\/([^/]+)\//);
    if (rtdMatch && slugs.indexOf(rtdMatch[1]) !== -1) {
      return path.replace(/^\/[^/]+\//, "/" + dest + "/");
    }
    var localMatch = path.match(/\/build\/([^/]+)\//);
    if (localMatch && slugs.indexOf(localMatch[1]) !== -1) {
      return path.replace(/\/build\/[^/]+\//, "/build/" + dest + "/");
    }
    var re = new RegExp("/(" + slugs.join("|") + ")/");
    if (re.test(path)) {
      return path.replace(re, "/" + dest + "/");
    }
    if (dest !== "en" && dest !== "html") {
      return path.replace(/^\//, "/" + dest + "/");
    }
    return path;
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

    var slugs = Array.prototype.map.call(selects[0].options, function (option) {
      return option.value;
    });
    slugs.push("html");

    Array.prototype.forEach.call(selects, function (select) {
      select.addEventListener("change", function () {
        var loc = window.location;
        loc.assign(
          switchUrl(loc, rewritePath(loc.pathname, slugs, select.value))
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
