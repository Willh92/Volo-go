export function BrandLogo({
  size = 34,
  variant = "default",
}: {
  size?: number;
  variant?: "default" | "rail";
}) {
  const isRail = variant === "rail";
  const bg = isRail ? "var(--volo-nav-mark, #9bcab1)" : "var(--volo-btn-bg, #246b51)";
  const fg = isRail ? "var(--volo-nav-mark-ink, #102a27)" : "#eef5ee";

  return (
    <svg
      viewBox="0 0 64 64"
      width={size}
      height={size}
      fill="none"
      aria-hidden="true"
      style={{ display: "block", flexShrink: 0, borderRadius: `${Math.round(size * 0.27)}px` }}
    >
      <rect x="2" y="2" width="60" height="60" rx="16" fill={bg} />
      <path
        d="M14.5 17.5H24.2L30.8 40.4L37.5 17.5H46.8L35.0 48.5H26.4L14.5 17.5Z"
        fill={fg}
      />
      <circle cx="48.5" cy="44.2" r="4.3" fill={fg} />
    </svg>
  );
}
