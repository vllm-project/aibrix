import { useState, useRef, useEffect, useCallback } from "react";
import {
  Settings,
  Globe,
  HelpCircle,
  Sparkles,
  Gift,
  Download,
  BookOpen,
  LogOut,
  ChevronRight,
  ChevronUp,
  Check,
  ExternalLink,
} from "lucide-react";

type Language = "en" | "zh";

interface UserMenuProps {
  userName?: string;
  userEmail?: string;
  planName?: string;
}

const languageOptions: { code: Language; label: string }[] = [
  { code: "en", label: "English (United States)" },
  { code: "zh", label: "中文 (简体)" },
];

type LearnMoreItem =
  | { label: string; href: string; shortcut?: string }
  | { type: "separator" };

const learnMoreLinks: LearnMoreItem[] = [
  { label: "Documentation", href: "https://aibrix.ai/docs" },
  { label: "GitHub", href: "https://github.com/aibrix/aibrix" },
  { type: "separator" },
  { label: "Usage policy", href: "#" },
  { label: "Privacy policy", href: "#" },
  { type: "separator" },
  { label: "Keyboard shortcuts", href: "#", shortcut: "⌘/" },
];

export function UserMenu({
  userName = "Test User",
  userEmail = "test@aibrix.ai",
  planName = "Test Plan",
}: UserMenuProps) {
  const [isOpen, setIsOpen] = useState(false);
  const [activeSubmenu, setActiveSubmenu] = useState<
    "language" | "learnMore" | null
  >(null);
  const [selectedLanguage, setSelectedLanguage] = useState<Language>(() => {
    try {
      return (localStorage.getItem("app-language") as Language) || "en";
    } catch {
      return "en";
    }
  });
  const containerRef = useRef<HTMLDivElement>(null);

  const closeMenu = useCallback(() => {
    setIsOpen(false);
    setActiveSubmenu(null);
  }, []);

  useEffect(() => {
    function handleClickOutside(e: MouseEvent) {
      if (
        containerRef.current &&
        !containerRef.current.contains(e.target as Node)
      ) {
        closeMenu();
      }
    }
    function handleEscape(e: KeyboardEvent) {
      if (e.key === "Escape") closeMenu();
    }
    if (isOpen) {
      document.addEventListener("mousedown", handleClickOutside);
      document.addEventListener("keydown", handleEscape);
    }
    return () => {
      document.removeEventListener("mousedown", handleClickOutside);
      document.removeEventListener("keydown", handleEscape);
    };
  }, [isOpen, closeMenu]);

  const handleLanguageChange = (lang: Language) => {
    setSelectedLanguage(lang);
    try {
      localStorage.setItem("app-language", lang);
    } catch {}
    closeMenu();
  };

  const userInitial = userName.charAt(0).toUpperCase();

  return (
    <div ref={containerRef} className="relative px-3 pb-3">
      {/* Popup Menu */}
      {isOpen && (
        <div className="absolute bottom-full left-2 right-2 mb-1 z-50">
          {/* Main menu */}
          <div
            className="rounded-xl border border-white/10 shadow-2xl"
            style={{ backgroundColor: "#352f29" }}
          >
            {/* Email */}
            <div className="px-4 py-2.5 text-xs text-sidebar-foreground/50 truncate border-b border-white/5">
              {userEmail}
            </div>

            {/* Section 1 */}
            <div className="py-1">
              <MenuItem
                icon={<Settings size={16} />}
                label="Settings"
                shortcut="⇧⌘,"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
              <div
                className="relative"
                onMouseEnter={() => setActiveSubmenu("language")}
              >
                <MenuItem
                  icon={<Globe size={16} />}
                  label="Language"
                  hasSubmenu
                  isActive={activeSubmenu === "language"}
                />
                {/* Language Submenu */}
                {activeSubmenu === "language" && (
                  <div
                    className="absolute left-full top-0 ml-1 rounded-xl border border-white/10 shadow-2xl min-w-[200px]"
                    style={{ backgroundColor: "#352f29" }}
                  >
                    <div className="py-1">
                      {languageOptions.map((lang) => (
                        <button
                          key={lang.code}
                          onClick={() => handleLanguageChange(lang.code)}
                          className="flex items-center w-full px-4 py-2 text-sm text-sidebar-foreground/80 hover:bg-white/5 transition-colors"
                        >
                          <span className="w-5 flex-shrink-0">
                            {selectedLanguage === lang.code && (
                              <Check
                                size={16}
                                className="text-sidebar-foreground/60"
                              />
                            )}
                          </span>
                          <span className="flex-1 text-center">{lang.label}</span>
                          <span className="w-5 flex-shrink-0" />
                        </button>
                      ))}
                    </div>
                  </div>
                )}
              </div>
              <MenuItem
                icon={<HelpCircle size={16} />}
                label="Get help"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
            </div>

            {/* Section 2 */}
            <div className="border-t border-white/5 py-1">
              <MenuItem
                icon={<Sparkles size={16} />}
                label="Upgrade plan"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
              <MenuItem
                icon={<Gift size={16} />}
                label="Gift AIBrix Chat"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
              <MenuItem
                icon={<Download size={16} />}
                label="Download Cowork"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
              <div
                className="relative"
                onMouseEnter={() => setActiveSubmenu("learnMore")}
              >
                <MenuItem
                  icon={<BookOpen size={16} />}
                  label="Learn more"
                  hasSubmenu
                  isActive={activeSubmenu === "learnMore"}
                />
                {/* Learn More Submenu */}
                {activeSubmenu === "learnMore" && (
                  <div
                    className="absolute left-full bottom-0 ml-1 rounded-xl border border-white/10 shadow-2xl min-w-[210px]"
                    style={{ backgroundColor: "#352f29" }}
                  >
                    <div className="py-1">
                      {learnMoreLinks.map((item, i) => {
                        if ("type" in item) {
                          return (
                            <div
                              key={`sep-${i}`}
                              className="my-1 border-t border-white/5"
                            />
                          );
                        }
                        return (
                          <a
                            key={item.label}
                            href={item.href}
                            target={item.href !== "#" ? "_blank" : undefined}
                            rel="noopener noreferrer"
                            className="flex items-center justify-between w-full px-4 py-2 text-sm text-sidebar-foreground/80 hover:bg-white/5 transition-colors"
                            onClick={closeMenu}
                          >
                            <span>{item.label}</span>
                            {item.shortcut ? (
                              <span className="text-xs text-sidebar-foreground/40 ml-4">
                                {item.shortcut}
                              </span>
                            ) : item.href !== "#" ? (
                              <ExternalLink
                                size={14}
                                className="text-sidebar-foreground/40 ml-4"
                              />
                            ) : null}
                          </a>
                        );
                      })}
                    </div>
                  </div>
                )}
              </div>
            </div>

            {/* Section 3 */}
            <div className="border-t border-white/5 py-1">
              <MenuItem
                icon={<LogOut size={16} />}
                label="Log out"
                onClick={closeMenu}
                onMouseEnter={() => setActiveSubmenu(null)}
              />
            </div>
          </div>
        </div>
      )}

      {/* User Profile Button */}
      <button
        onClick={() => {
          setIsOpen(!isOpen);
          if (isOpen) setActiveSubmenu(null);
        }}
        className="flex items-center gap-2.5 w-full px-2 py-2 rounded-lg hover:bg-sidebar-accent transition-colors"
      >
        <div className="w-7 h-7 rounded-full bg-[#c4a882] flex items-center justify-center shrink-0">
          <span className="text-sm text-[#1a1714]" style={{ lineHeight: 1 }}>
            {userInitial}
          </span>
        </div>
        <div className="flex-1 min-w-0 text-left">
          <div className="text-sm text-sidebar-foreground truncate leading-tight">
            {userName}
          </div>
          <div className="text-xs text-sidebar-foreground/40 truncate leading-tight">
            {planName}
          </div>
        </div>
        <ChevronUp
          size={14}
          className={`text-sidebar-foreground/40 transition-transform ${
            isOpen ? "" : "rotate-180"
          }`}
        />
      </button>
    </div>
  );
}

/* ---- Reusable Menu Item ---- */
interface MenuItemProps {
  icon: React.ReactNode;
  label: string;
  shortcut?: string;
  hasSubmenu?: boolean;
  isActive?: boolean;
  onClick?: () => void;
  onMouseEnter?: () => void;
}

function MenuItem({
  icon,
  label,
  shortcut,
  hasSubmenu,
  isActive,
  onClick,
  onMouseEnter,
}: MenuItemProps) {
  return (
    <button
      onClick={onClick}
      onMouseEnter={onMouseEnter}
      className={`flex items-center gap-3 w-full px-4 py-2 text-sm transition-colors ${
        isActive
          ? "bg-white/5 text-sidebar-foreground"
          : "text-sidebar-foreground/80 hover:bg-white/5 hover:text-sidebar-foreground"
      }`}
    >
      <span className="text-sidebar-foreground/60">{icon}</span>
      <span className="flex-1 text-left">{label}</span>
      {shortcut && (
        <span className="text-xs text-sidebar-foreground/30">{shortcut}</span>
      )}
      {hasSubmenu && (
        <ChevronRight size={14} className="text-sidebar-foreground/40" />
      )}
    </button>
  );
}
