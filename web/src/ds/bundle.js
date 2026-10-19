// @ts-nocheck
// Vendored from the Azrty design system bundle (components only).
// Local patches: accessible names for action column headers, portal-rendered dropdown menus, keyboard-operable table rows, dialog focus management, unique field ids.
import * as React from "react";
import { createPortal } from "react-dom";
const __ds_ns = { __errors: [] };
const __ds_scope = {};
function useDialogFocus(open, inline) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (!open || inline || !ref.current) return;
    const node = ref.current;
    const previous = document.activeElement;
    const sel = 'a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])';
    const body = node.querySelector('.az-modal__body, .az-drawer__body');
    const first = body && body.querySelector(sel);
    (first || node).focus();
    const trap = e => {
      if (e.key !== 'Tab') return;
      const items = Array.from(node.querySelectorAll(sel)).filter(el => el.offsetParent !== null || el === document.activeElement);
      if (!items.length) { e.preventDefault(); node.focus(); return; }
      const a = items[0], z = items[items.length - 1];
      if (e.shiftKey && (document.activeElement === a || document.activeElement === node)) { e.preventDefault(); z.focus(); }
      else if (!e.shiftKey && document.activeElement === z) { e.preventDefault(); a.focus(); }
    };
    node.addEventListener('keydown', trap);
    return () => {
      node.removeEventListener('keydown', trap);
      if (previous && previous.focus && document.contains(previous)) previous.focus();
    };
  }, [open, inline]);
  return ref;
}
// components/brand/Icon.jsx
try { (() => {
function Icon({
  name,
  size = 16,
  color,
  className = '',
  style,
  label
}) {
  return /*#__PURE__*/React.createElement("i", {
    className: ('icon-' + name + ' az-icon ' + className).trim(),
    style: {
      fontSize: size,
      color,
      ...style
    },
    "aria-hidden": label ? undefined : 'true',
    "aria-label": label,
    role: label ? 'img' : undefined
  });
}
Object.assign(__ds_scope, { Icon });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/brand/Icon.jsx", error: String((e && e.message) || e) }); }

// components/actions/Button.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Button({
  variant = 'primary',
  size = 'md',
  icon,
  iconRight,
  block,
  children,
  className = '',
  type = 'button',
  ...rest
}) {
  const cls = ['az-btn', 'az-btn--' + variant, size !== 'md' && 'az-btn--' + size, block && 'az-btn--block', !children && 'az-btn--icon-only', className].filter(Boolean).join(' ');
  const is = size === 'sm' ? 13 : size === 'lg' ? 17 : 15;
  return /*#__PURE__*/React.createElement("button", _extends({
    type: type,
    className: cls
  }, rest), icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: is
  }), children, iconRight && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: iconRight,
    size: is
  }));
}
Object.assign(__ds_scope, { Button });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/actions/Button.jsx", error: String((e && e.message) || e) }); }

// components/actions/IconButton.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function IconButton({
  icon,
  label,
  size = 16,
  className = '',
  type = 'button',
  ...rest
}) {
  return /*#__PURE__*/React.createElement("button", _extends({
    type: type,
    className: ('az-iconbtn ' + className).trim(),
    "aria-label": label,
    title: label
  }, rest), /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: size
  }));
}
Object.assign(__ds_scope, { IconButton });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/actions/IconButton.jsx", error: String((e && e.message) || e) }); }

// components/brand/Logo.jsx
try { (() => {
function Logo({
  variant = 'lockup',
  size = 32,
  className = '',
  style
}) {
  const stroke = size <= 20 ? 6.5 : 5.5;
  const mark = /*#__PURE__*/React.createElement("svg", {
    width: size,
    height: size,
    viewBox: "0 0 40 40",
    "aria-hidden": "true",
    style: {
      display: 'block',
      flex: 'none'
    }
  }, /*#__PURE__*/React.createElement("circle", {
    cx: "20",
    cy: "20",
    r: "20",
    style: {
      fill: 'var(--az-brand-sky)'
    }
  }), /*#__PURE__*/React.createElement("polyline", {
    points: "16,11 25.5,20 16,29",
    fill: "none",
    strokeWidth: stroke,
    strokeLinecap: "round",
    strokeLinejoin: "round",
    style: {
      stroke: 'var(--az-brand-mint)'
    }
  }));
  if (variant === 'mark') return /*#__PURE__*/React.createElement("span", {
    className: className,
    style: {
      display: 'inline-flex',
      ...style
    },
    role: "img",
    "aria-label": "Azrty"
  }, mark);
  return /*#__PURE__*/React.createElement("span", {
    className: ('az-logo ' + className).trim(),
    style: {
      gap: Math.round(size * 0.33),
      ...style
    },
    role: "img",
    "aria-label": "Azrty"
  }, mark, /*#__PURE__*/React.createElement("span", {
    className: "az-logo__word",
    style: {
      fontSize: Math.round(size * 0.82)
    }
  }, "AZRTY"));
}
Object.assign(__ds_scope, { Logo });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/brand/Logo.jsx", error: String((e && e.message) || e) }); }

// components/brand/ProductLogo.jsx
try { (() => {
function ProductLogo({
  name,
  sub,
  tagline,
  pillar,
  emblem,
  emblemLight,
  layout = 'stacked',
  size = 160,
  className = '',
  style
}) {
  const label = [name, sub].filter(Boolean).join(' ');
  const em = emblem ? /*#__PURE__*/React.createElement(React.Fragment, null, /*#__PURE__*/React.createElement("img", {
    className: "az-plogo__em az-plogo__em--dark",
    src: emblem,
    alt: ""
  }), /*#__PURE__*/React.createElement("img", {
    className: "az-plogo__em az-plogo__em--light",
    src: emblemLight || emblem,
    alt: ""
  })) : /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__ph"
  }, "Emblem");
  const s = layout === 'horizontal' ? Math.round(size) : size;
  return /*#__PURE__*/React.createElement("span", {
    className: 'az-plogo az-plogo--' + layout + (className ? ' ' + className : ''),
    "data-pillar": pillar,
    role: "img",
    "aria-label": label,
    style: {
      '--az-plogo-size': s + 'px',
      ...style
    }
  }, em, layout !== 'icon' && /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__text"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__name"
  }, name), sub && /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__sub"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__subtext"
  }, sub)), tagline && layout === 'stacked' && /*#__PURE__*/React.createElement("span", {
    className: "az-plogo__tag"
  }, tagline)));
}
Object.assign(__ds_scope, { ProductLogo });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/brand/ProductLogo.jsx", error: String((e && e.message) || e) }); }

// components/controls/Checkbox.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Checkbox({
  label,
  hint,
  indeterminate,
  disabled,
  className = '',
  style,
  ...rest
}) {
  const ref = React.useRef(null);
  React.useEffect(() => {
    if (ref.current) ref.current.indeterminate = !!indeterminate;
  }, [indeterminate]);
  return /*#__PURE__*/React.createElement("label", {
    className: 'az-check' + (disabled ? ' az-check--disabled' : '') + (className ? ' ' + className : ''),
    style: style
  }, /*#__PURE__*/React.createElement("input", _extends({
    ref: ref,
    type: "checkbox",
    className: "az-check__input",
    disabled: disabled
  }, rest)), /*#__PURE__*/React.createElement("span", {
    className: "az-check__box"
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: indeterminate ? 'minus' : 'check',
    size: 12
  })), (label || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-check__text"
  }, label && /*#__PURE__*/React.createElement("span", {
    className: "az-check__label"
  }, label), hint && /*#__PURE__*/React.createElement("span", {
    className: "az-check__hint"
  }, hint)));
}
Object.assign(__ds_scope, { Checkbox });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/controls/Checkbox.jsx", error: String((e && e.message) || e) }); }

// components/controls/Radio.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Radio({
  label,
  hint,
  disabled,
  className = '',
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("label", {
    className: 'az-check' + (disabled ? ' az-check--disabled' : '') + (className ? ' ' + className : ''),
    style: style
  }, /*#__PURE__*/React.createElement("input", _extends({
    type: "radio",
    className: "az-check__input",
    disabled: disabled
  }, rest)), /*#__PURE__*/React.createElement("span", {
    className: "az-check__box az-check__box--radio"
  }), (label || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-check__text"
  }, label && /*#__PURE__*/React.createElement("span", {
    className: "az-check__label"
  }, label), hint && /*#__PURE__*/React.createElement("span", {
    className: "az-check__hint"
  }, hint)));
}
function RadioGroup({
  label,
  name,
  options = [],
  value,
  defaultValue,
  onChange,
  row,
  className = '',
  style
}) {
  const [inner, setInner] = React.useState(defaultValue ?? (options[0] && (options[0].value ?? options[0])));
  const cur = value !== undefined ? value : inner;
  return /*#__PURE__*/React.createElement("fieldset", {
    className: 'az-radio-group' + (row ? ' az-radio-group--row' : '') + (className ? ' ' + className : ''),
    style: style
  }, label && /*#__PURE__*/React.createElement("legend", {
    className: "az-radio-group__legend"
  }, label), options.map(o => {
    const opt = typeof o === 'string' ? {
      value: o,
      label: o
    } : o;
    return /*#__PURE__*/React.createElement(Radio, {
      key: opt.value,
      name: name,
      value: opt.value,
      label: opt.label,
      hint: opt.hint,
      disabled: opt.disabled,
      checked: cur === opt.value,
      onChange: () => {
        setInner(opt.value);
        onChange && onChange(opt.value);
      }
    });
  }));
}
Object.assign(__ds_scope, { Radio, RadioGroup });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/controls/Radio.jsx", error: String((e && e.message) || e) }); }

// components/controls/Switch.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Switch({
  label,
  hint,
  labelPosition = 'start',
  disabled,
  className = '',
  style,
  ...rest
}) {
  return /*#__PURE__*/React.createElement("label", {
    className: 'az-check az-check--switch' + (labelPosition === 'end' ? ' az-check--end' : '') + (disabled ? ' az-check--disabled' : '') + (className ? ' ' + className : ''),
    style: style
  }, /*#__PURE__*/React.createElement("input", _extends({
    type: "checkbox",
    role: "switch",
    className: "az-check__input",
    disabled: disabled
  }, rest)), /*#__PURE__*/React.createElement("span", {
    className: "az-switch__track"
  }), (label || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-check__text"
  }, label && /*#__PURE__*/React.createElement("span", {
    className: "az-check__label"
  }, label), hint && /*#__PURE__*/React.createElement("span", {
    className: "az-check__hint"
  }, hint)));
}
Object.assign(__ds_scope, { Switch });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/controls/Switch.jsx", error: String((e && e.message) || e) }); }

// components/data/ChartLegend.jsx
try { (() => {
function ChartLegend({
  items = [],
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-legend ' + className).trim(),
    style: style
  }, items.map((it, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    className: "az-legend__item"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-legend__swatch",
    style: {
      background: it.color || 'var(--az-chart-' + (i + 1) + ')'
    }
  }), it.label, it.value != null && /*#__PURE__*/React.createElement("span", {
    className: "az-legend__value"
  }, it.value))));
}
Object.assign(__ds_scope, { ChartLegend });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/ChartLegend.jsx", error: String((e && e.message) || e) }); }

// components/data/CodeBlock.jsx
try { (() => {
function CodeBlock({
  code = '',
  title,
  copyable = true,
  maxHeight,
  className = '',
  style
}) {
  const [copied, setCopied] = React.useState(false);
  const copy = () => {
    if (navigator.clipboard) navigator.clipboard.writeText(code).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1400);
  };
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-code ' + className).trim(),
    style: style
  }, (title || copyable) && /*#__PURE__*/React.createElement("div", {
    className: "az-code__head"
  }, /*#__PURE__*/React.createElement("span", null, title), copyable && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: copied ? 'check' : 'copy',
    label: copied ? 'Copied' : 'Copy',
    size: 14,
    onClick: copy
  })), /*#__PURE__*/React.createElement("pre", {
    style: {
      maxHeight
    }
  }, code));
}
Object.assign(__ds_scope, { CodeBlock });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/CodeBlock.jsx", error: String((e && e.message) || e) }); }

// components/data/LineChart.jsx
try { (() => {
function LineChart({
  series = [],
  labels = [],
  height = 160,
  yTicks = 4,
  format = v => String(v),
  area = true,
  className = '',
  style
}) {
  const all = series.flatMap(s => s.data);
  const max = Math.max(1, ...all) * 1.1;
  const n = Math.max(2, ...series.map(s => s.data.length));
  const x = i => i / (n - 1) * 100;
  const y = v => height - v / max * height;
  const ticks = Array.from({
    length: yTicks + 1
  }, (_, i) => max * (yTicks - i) / yTicks);
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-chart ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-chart__scale",
    style: {
      height
    }
  }, ticks.map((t, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    style: {
      top: y(t) / height * 100 + '%'
    }
  }, format(Math.round(t))))), /*#__PURE__*/React.createElement("svg", {
    viewBox: '0 0 100 ' + height,
    preserveAspectRatio: "none",
    style: {
      width: '100%',
      height,
      display: 'block',
      overflow: 'visible'
    },
    role: "img"
  }, ticks.map((t, i) => /*#__PURE__*/React.createElement("line", {
    key: i,
    x1: "0",
    x2: "100",
    y1: y(t),
    y2: y(t),
    strokeWidth: "1",
    vectorEffect: "non-scaling-stroke",
    strokeDasharray: i === yTicks ? undefined : '2 4',
    style: {
      stroke: 'var(--az-border)'
    }
  })), series.map((s, si) => {
    const c = s.color || 'var(--az-chart-' + (si + 1) + ')';
    const pts = s.data.map((v, i) => x(i) + ',' + y(v)).join(' ');
    return /*#__PURE__*/React.createElement("g", {
      key: si
    }, area && si === 0 && /*#__PURE__*/React.createElement("polygon", {
      points: '0,' + height + ' ' + pts + ' ' + x(s.data.length - 1) + ',' + height,
      style: {
        fill: c,
        opacity: 0.12
      }
    }), /*#__PURE__*/React.createElement("polyline", {
      points: pts,
      fill: "none",
      strokeWidth: "2",
      strokeLinejoin: "round",
      strokeLinecap: "round",
      vectorEffect: "non-scaling-stroke",
      style: {
        stroke: c
      }
    }));
  })), labels.length > 0 && /*#__PURE__*/React.createElement("div", {
    className: "az-chart__x"
  }, labels.map((l, i) => /*#__PURE__*/React.createElement("span", {
    key: i
  }, l))));
}
Object.assign(__ds_scope, { LineChart });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/LineChart.jsx", error: String((e && e.message) || e) }); }

// components/data/Meter.jsx
try { (() => {
const METER_TONES = {
  mint: 'var(--az-brand-mint)',
  sky: 'var(--az-brand-sky)',
  'sky-light': 'var(--az-sky-light)',
  warn: 'var(--az-warn)',
  bad: 'var(--az-bad)'
};
function Meter({
  value = 0,
  max = 100,
  segments,
  tone = 'mint',
  label,
  valueLabel,
  size = 'md',
  className = '',
  style
}) {
  const segs = segments || [{
    value,
    tone
  }];
  const base = tone === 'auto' ? 'mint' : tone;
  const auto = value / max >= 0.9 ? 'bad' : value / max >= 0.75 ? 'warn' : base;
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-meter' + (size === 'lg' ? ' az-meter--lg ' : ' ') + className).trim(),
    style: style
  }, (label || valueLabel) && /*#__PURE__*/React.createElement("div", {
    className: "az-meter__head"
  }, /*#__PURE__*/React.createElement("span", null, label), valueLabel && /*#__PURE__*/React.createElement("span", {
    className: "az-meter__value"
  }, valueLabel)), /*#__PURE__*/React.createElement("div", {
    className: "az-meter__track",
    role: "meter",
    "aria-valuenow": value,
    "aria-valuemin": 0,
    "aria-valuemax": max,
    "aria-label": label
  }, segs.map((s, i) => /*#__PURE__*/React.createElement("span", {
    key: i,
    className: "az-meter__bar",
    style: {
      width: Math.max(0, Math.min(100, s.value / max * 100)) + '%',
      background: METER_TONES[segments ? s.tone || 'mint' : tone === 'auto' ? auto : base] || s.tone
    }
  }))));
}
Object.assign(__ds_scope, { Meter });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/Meter.jsx", error: String((e && e.message) || e) }); }

// components/data/Sparkline.jsx
try { (() => {
function Sparkline({
  data = [],
  color = 'var(--az-brand-mint)',
  height = 45,
  fill = true,
  className = '',
  style
}) {
  if (data.length < 2) return /*#__PURE__*/React.createElement("div", {
    className: className,
    style: {
      height,
      ...style
    }
  });
  const max = Math.max(...data),
    min = Math.min(...data),
    span = max - min || 1;
  const pts = data.map((v, i) => i / (data.length - 1) * 100 + ',' + (height - 2 - (v - min) / span * (height - 4))).join(' ');
  return /*#__PURE__*/React.createElement("svg", {
    className: className,
    viewBox: '0 0 100 ' + height,
    preserveAspectRatio: "none",
    style: {
      width: '100%',
      height,
      display: 'block',
      overflow: 'visible',
      ...style
    },
    "aria-hidden": "true"
  }, fill && /*#__PURE__*/React.createElement("polygon", {
    points: '0,' + height + ' ' + pts + ' 100,' + height,
    style: {
      fill: color,
      opacity: 0.14
    }
  }), /*#__PURE__*/React.createElement("polyline", {
    points: pts,
    fill: "none",
    strokeWidth: "1.75",
    strokeLinejoin: "round",
    strokeLinecap: "round",
    vectorEffect: "non-scaling-stroke",
    style: {
      stroke: color
    }
  }));
}
Object.assign(__ds_scope, { Sparkline });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/Sparkline.jsx", error: String((e && e.message) || e) }); }

// components/data/StatCard.jsx
try { (() => {
function StatCard({
  label,
  value,
  unit,
  icon,
  sub,
  delta,
  trend,
  trendColor,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-card az-stat ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-stat__head"
  }, /*#__PURE__*/React.createElement("span", null, label), icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: 15
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-stat__value"
  }, value, unit && /*#__PURE__*/React.createElement("span", {
    className: "az-stat__unit"
  }, unit)), (sub || delta) && /*#__PURE__*/React.createElement("div", {
    className: "az-stat__sub"
  }, delta && /*#__PURE__*/React.createElement("span", {
    className: 'az-stat__delta--' + (delta.direction || 'up')
  }, delta.value), sub && /*#__PURE__*/React.createElement("span", null, sub)), trend && /*#__PURE__*/React.createElement("div", {
    className: "az-stat__spark"
  }, /*#__PURE__*/React.createElement(__ds_scope.Sparkline, {
    data: trend,
    color: trendColor,
    height: 36
  })));
}
Object.assign(__ds_scope, { StatCard });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/StatCard.jsx", error: String((e && e.message) || e) }); }

// components/data/Table.jsx
try { (() => {
function Table({
  columns = [],
  rows = [],
  onRowClick,
  rowKey = 'id',
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-table-wrap ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("table", {
    className: "az-table"
  }, /*#__PURE__*/React.createElement("thead", null, /*#__PURE__*/React.createElement("tr", null, columns.map(c => /*#__PURE__*/React.createElement("th", {
    key: c.key,
    style: {
      textAlign: c.align || 'left',
      width: c.width
    }
  }, c.label === '' ? /*#__PURE__*/React.createElement("span", {
    className: "az-sr-only"
  }, "Actions") : c.label)))), /*#__PURE__*/React.createElement("tbody", null, rows.map((r, i) => /*#__PURE__*/React.createElement("tr", {
    key: r[rowKey] != null ? r[rowKey] : i,
    className: onRowClick ? 'az-table__row--click' : undefined,
    tabIndex: onRowClick ? 0 : undefined,
    onClick: onRowClick ? () => onRowClick(r) : undefined,
    onKeyDown: onRowClick ? e => {
      if (e.target === e.currentTarget && (e.key === 'Enter' || e.key === ' ')) {
        e.preventDefault();
        onRowClick(r);
      }
    } : undefined
  }, columns.map((c, ci) => /*#__PURE__*/React.createElement("td", {
    key: c.key,
    style: {
      textAlign: c.align || 'left'
    },
    className: [ci === 0 && c.primary !== false ? 'az-table__primary' : '', c.mono ? 'az-table__mono' : ''].join(' ').trim() || undefined
  }, c.render ? c.render(r) : r[c.key])))))));
}
Object.assign(__ds_scope, { Table });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/data/Table.jsx", error: String((e && e.message) || e) }); }

// components/disclosure/Accordion.jsx
try { (() => {
function Accordion({
  items = [],
  multiple,
  defaultOpen = [],
  className = '',
  style
}) {
  const [open, setOpen] = React.useState(defaultOpen);
  const toggle = id => setOpen(o => o.includes(id) ? o.filter(x => x !== id) : multiple ? [...o, id] : [id]);
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-accordion ' + className).trim(),
    style: style
  }, items.map(it => {
    const isOpen = open.includes(it.id);
    return /*#__PURE__*/React.createElement("div", {
      key: it.id,
      className: 'az-acc__item' + (isOpen ? ' az-acc__item--open' : '')
    }, /*#__PURE__*/React.createElement("button", {
      className: "az-acc__trigger",
      "aria-expanded": isOpen,
      onClick: () => toggle(it.id)
    }, it.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: it.icon,
      size: 15,
      style: {
        color: 'var(--az-muted)'
      }
    }), /*#__PURE__*/React.createElement("span", null, it.title), it.meta && /*#__PURE__*/React.createElement("span", {
      className: "az-acc__meta"
    }, it.meta), /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: "chevron-down",
      size: 15,
      className: "az-acc__chev",
      style: it.meta ? {
        marginLeft: 12
      } : {
        marginLeft: 'auto'
      }
    })), /*#__PURE__*/React.createElement("div", {
      className: "az-acc__panel"
    }, /*#__PURE__*/React.createElement("div", {
      className: "az-acc__inner"
    }, /*#__PURE__*/React.createElement("div", {
      className: "az-acc__content"
    }, it.content))));
  }));
}
Object.assign(__ds_scope, { Accordion });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/disclosure/Accordion.jsx", error: String((e && e.message) || e) }); }

// components/disclosure/Carousel.jsx
try { (() => {
function Carousel({
  children,
  perView = 1,
  gap = 16,
  loop,
  autoPlay,
  showCount = true,
  className = '',
  style,
  'aria-label': ariaLabel
}) {
  const slides = React.Children.toArray(children);
  const pages = Math.max(1, slides.length - perView + 1);
  const [i, setI] = React.useState(0);
  const go = n => setI(loop ? (n + pages) % pages : Math.min(pages - 1, Math.max(0, n)));
  React.useEffect(() => {
    if (!autoPlay) return;
    const t = setInterval(() => setI(x => (x + 1) % pages), autoPlay);
    return () => clearInterval(t);
  }, [autoPlay, pages]);
  const w = 'calc((100% - ' + gap * (perView - 1) + 'px) / ' + perView + ')';
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-carousel ' + className).trim(),
    role: "region",
    "aria-roledescription": "carousel",
    "aria-label": ariaLabel,
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-carousel__viewport"
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-carousel__track",
    style: {
      gap,
      transform: 'translateX(calc(' + -i + ' * (' + w + ' + ' + gap + 'px)))'
    }
  }, slides.map((s, k) => /*#__PURE__*/React.createElement("div", {
    key: k,
    className: "az-carousel__slide",
    style: {
      width: w
    },
    "aria-hidden": k < i || k >= i + perView ? 'true' : undefined
  }, s)))), pages > 1 && /*#__PURE__*/React.createElement("div", {
    className: "az-carousel__controls"
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-carousel__dots"
  }, Array.from({
    length: pages
  }, (_, k) => /*#__PURE__*/React.createElement("button", {
    key: k,
    className: 'az-carousel__dot' + (k === i ? ' az-carousel__dot--active' : ''),
    "aria-label": 'Go to slide ' + (k + 1),
    onClick: () => go(k)
  }))), /*#__PURE__*/React.createElement("div", {
    className: "az-carousel__arrows"
  }, showCount && /*#__PURE__*/React.createElement("span", {
    className: "az-carousel__count"
  }, String(i + 1).padStart(2, '0'), " / ", String(pages).padStart(2, '0')), /*#__PURE__*/React.createElement("button", {
    className: "az-iconbtn",
    "aria-label": "Previous",
    disabled: !loop && i === 0,
    onClick: () => go(i - 1),
    style: !loop && i === 0 ? {
      opacity: 0.4,
      cursor: 'not-allowed'
    } : undefined
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-left",
    size: 16
  })), /*#__PURE__*/React.createElement("button", {
    className: "az-iconbtn",
    "aria-label": "Next",
    disabled: !loop && i === pages - 1,
    onClick: () => go(i + 1),
    style: !loop && i === pages - 1 ? {
      opacity: 0.4,
      cursor: 'not-allowed'
    } : undefined
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-right",
    size: 16
  })))));
}
Object.assign(__ds_scope, { Carousel });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/disclosure/Carousel.jsx", error: String((e && e.message) || e) }); }

// components/disclosure/DropdownMenu.jsx
try { (() => {
function DropdownMenu({
  trigger,
  items = [],
  align = 'start',
  open: forced,
  className = '',
  style
}) {
  const [open, setOpen] = React.useState(false);
  const [pos, setPos] = React.useState(null);
  const ref = React.useRef(null);
  const menuRef = React.useRef(null);
  const isOpen = forced != null ? forced : open;
  // The menu is rendered in a portal on document.body with fixed coordinates so it
  // is never clipped by an overflow container (tables, drawers, cards).
  const place = React.useCallback(() => {
    if (!ref.current) return;
    const r = ref.current.getBoundingClientRect();
    const m = menuRef.current;
    const w = m ? m.offsetWidth : 200;
    const h = m ? m.offsetHeight : 0;
    const vw = document.documentElement.clientWidth;
    const vh = window.innerHeight;
    let left = align === 'end' ? r.right - w : r.left;
    left = Math.max(8, Math.min(left, vw - w - 8));
    let top = r.bottom + 6;
    if (h && top + h > vh - 8 && r.top - 6 - h >= 8) top = r.top - 6 - h;
    else if (h && top + h > vh - 8) top = Math.max(8, vh - h - 8);
    setPos(p => p && p.left === left && p.top === top ? p : { left, top });
  }, [align]);
  React.useLayoutEffect(() => {
    if (!isOpen) { setPos(null); return; }
    place();
  }, [isOpen, place, items.length]);
  React.useEffect(() => {
    if (!isOpen) return;
    const off = e => {
      if (ref.current && ref.current.contains(e.target)) return;
      if (menuRef.current && menuRef.current.contains(e.target)) return;
      setOpen(false);
    };
    const esc = e => {
      if (e.key === 'Escape') setOpen(false);
    };
    const move = e => {
      if (menuRef.current && e.target instanceof Node && menuRef.current.contains(e.target)) return;
      place();
    };
    document.addEventListener('mousedown', off);
    document.addEventListener('keydown', esc);
    window.addEventListener('resize', move);
    window.addEventListener('scroll', move, true);
    return () => {
      document.removeEventListener('mousedown', off);
      document.removeEventListener('keydown', esc);
      window.removeEventListener('resize', move);
      window.removeEventListener('scroll', move, true);
    };
  }, [isOpen, place]);
  const menu = isOpen && /*#__PURE__*/React.createElement("div", {
    ref: menuRef,
    className: 'az-menu az-menu--portal' + (align === 'end' ? ' az-menu--end' : ''),
    role: "menu",
    style: pos ? { top: pos.top, left: pos.left } : { top: 0, left: 0, visibility: 'hidden' },
    onClick: e => e.stopPropagation()
  }, items.map((it, k) => it.divider ? /*#__PURE__*/React.createElement("div", {
    key: k,
    className: "az-menu__sep",
    role: "separator"
  }) : it.heading ? /*#__PURE__*/React.createElement("div", {
    key: k,
    className: "az-menu__label"
  }, it.heading) : /*#__PURE__*/React.createElement("button", {
    key: k,
    role: "menuitem",
    disabled: it.disabled,
    className: 'az-menu__item' + (it.danger ? ' az-menu__item--danger' : ''),
    onClick: () => {
      setOpen(false);
      it.onSelect && it.onSelect();
    }
  }, it.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: it.icon,
    size: 15
  }), it.label, it.kbd && /*#__PURE__*/React.createElement("span", {
    className: "az-menu__kbd"
  }, it.kbd))));
  return /*#__PURE__*/React.createElement("div", {
    ref: ref,
    className: ('az-menu-wrap ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("span", {
    onClick: e => {
      e.stopPropagation();
      setOpen(o => !o);
    },
    style: {
      display: 'inline-flex'
    }
  }, trigger), menu && createPortal(menu, document.body));
}
Object.assign(__ds_scope, { DropdownMenu });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/disclosure/DropdownMenu.jsx", error: String((e && e.message) || e) }); }

// components/docs/Breadcrumbs.jsx
try { (() => {
function Breadcrumbs({
  items = [],
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("nav", {
    className: ('az-crumbs-nav ' + className).trim(),
    "aria-label": "Breadcrumb",
    style: style
  }, items.map((it, i) => {
    const last = i === items.length - 1;
    const c = typeof it === 'string' ? {
      label: it
    } : it;
    return /*#__PURE__*/React.createElement(React.Fragment, {
      key: i
    }, i > 0 && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: "chevron-right",
      size: 12
    }), last ? /*#__PURE__*/React.createElement("span", {
      "aria-current": "page"
    }, c.label) : /*#__PURE__*/React.createElement("a", {
      href: c.href || '#',
      onClick: c.onClick
    }, c.label));
  }));
}
Object.assign(__ds_scope, { Breadcrumbs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/Breadcrumbs.jsx", error: String((e && e.message) || e) }); }

// components/docs/Callout.jsx
try { (() => {
const CALLOUT_ICONS = {
  note: 'info',
  tip: 'lightbulb',
  warning: 'triangle-alert',
  danger: 'octagon-alert'
};
function Callout({
  kind = 'note',
  title,
  children,
  icon,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-callout az-callout--' + kind + (className ? ' ' + className : ''),
    role: "note",
    style: style
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon || CALLOUT_ICONS[kind],
    size: 16
  }), /*#__PURE__*/React.createElement("div", {
    className: "az-callout__body"
  }, title && /*#__PURE__*/React.createElement("span", {
    className: "az-callout__title"
  }, title), /*#__PURE__*/React.createElement("div", null, children)));
}
Object.assign(__ds_scope, { Callout });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/Callout.jsx", error: String((e && e.message) || e) }); }

// components/docs/CodeTabs.jsx
try { (() => {
function CodeTabs({
  tabs = [],
  defaultValue,
  copyable = true,
  maxHeight,
  className = '',
  style
}) {
  const [active, setActive] = React.useState(defaultValue || tabs[0] && tabs[0].label);
  const [copied, setCopied] = React.useState(false);
  const cur = tabs.find(t => t.label === active) || tabs[0] || {
    code: ''
  };
  const copy = () => {
    if (navigator.clipboard) navigator.clipboard.writeText(cur.code).catch(() => {});
    setCopied(true);
    setTimeout(() => setCopied(false), 1400);
  };
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-code az-codetabs ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-code__head"
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-codetabs__tabs",
    role: "tablist"
  }, tabs.map(t => /*#__PURE__*/React.createElement("button", {
    key: t.label,
    role: "tab",
    "aria-selected": t.label === cur.label,
    className: 'az-codetabs__tab' + (t.label === cur.label ? ' az-codetabs__tab--active' : ''),
    onClick: () => setActive(t.label)
  }, t.label))), copyable && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: copied ? 'check' : 'copy',
    label: copied ? 'Copied' : 'Copy',
    size: 14,
    onClick: copy
  })), /*#__PURE__*/React.createElement("pre", {
    style: {
      maxHeight
    }
  }, cur.code));
}
Object.assign(__ds_scope, { CodeTabs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/CodeTabs.jsx", error: String((e && e.message) || e) }); }

// components/docs/Heading.jsx
try { (() => {
function Heading({
  level = 2,
  id,
  children,
  anchor = true,
  className = '',
  style
}) {
  const Tag = 'h' + level;
  const text = typeof children === 'string' ? children : '';
  const hid = id || (text ? text.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/(^-|-$)/g, '') : undefined);
  return /*#__PURE__*/React.createElement(Tag, {
    id: hid,
    className: 'az-h az-h--' + level + (className ? ' ' + className : ''),
    style: style
  }, children, anchor && hid && level > 1 && /*#__PURE__*/React.createElement("a", {
    className: "az-h__anchor",
    href: '#' + hid,
    "aria-label": "Link to this section"
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "link",
    size: 14
  })));
}
Object.assign(__ds_scope, { Heading });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/Heading.jsx", error: String((e && e.message) || e) }); }

// components/docs/Kbd.jsx
try { (() => {
function Kbd({
  children,
  keys,
  className = '',
  style
}) {
  if (keys) return /*#__PURE__*/React.createElement("span", {
    className: ('az-kbd-combo ' + className).trim(),
    style: style
  }, keys.map(k => /*#__PURE__*/React.createElement("kbd", {
    key: k,
    className: "az-kbd"
  }, k)));
  return /*#__PURE__*/React.createElement("kbd", {
    className: ('az-kbd ' + className).trim(),
    style: style
  }, children);
}
Object.assign(__ds_scope, { Kbd });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/Kbd.jsx", error: String((e && e.message) || e) }); }

// components/docs/VersionBadge.jsx
try { (() => {
function VersionBadge({
  children,
  kind = 'version',
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("span", {
    className: 'az-ver' + (kind !== 'version' ? ' az-ver--' + kind : '') + (className ? ' ' + className : ''),
    style: style
  }, children);
}
Object.assign(__ds_scope, { VersionBadge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/VersionBadge.jsx", error: String((e && e.message) || e) }); }

// components/docs/ParamTable.jsx
try { (() => {
function ParamTable({
  params = [],
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-params ' + className).trim(),
    style: style
  }, params.map(p => /*#__PURE__*/React.createElement("div", {
    key: p.name,
    className: "az-param"
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-param__head"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-param__name"
  }, p.name), p.type && /*#__PURE__*/React.createElement("span", {
    className: "az-param__type"
  }, p.type), p.required && /*#__PURE__*/React.createElement("span", {
    className: "az-param__req"
  }, "required"), p.default != null && /*#__PURE__*/React.createElement("span", {
    className: "az-param__default"
  }, "default ", String(p.default)), p.badge && /*#__PURE__*/React.createElement(__ds_scope.VersionBadge, {
    kind: p.badgeKind || 'version'
  }, p.badge)), p.description && /*#__PURE__*/React.createElement("div", {
    className: "az-param__desc"
  }, p.description))));
}
Object.assign(__ds_scope, { ParamTable });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/docs/ParamTable.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Alert.jsx
try { (() => {
const ALERT_ICONS = {
  info: 'info',
  good: 'circle-check',
  warn: 'triangle-alert',
  bad: 'circle-x'
};
function Alert({
  tone = 'info',
  title,
  children,
  icon,
  action,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-alert az-alert--' + tone + ' ' + className).trim(),
    role: tone === 'bad' ? 'alert' : 'status',
    style: style
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon || ALERT_ICONS[tone],
    size: 16
  }), /*#__PURE__*/React.createElement("div", {
    className: "az-alert__body"
  }, title && /*#__PURE__*/React.createElement("span", {
    className: "az-alert__title"
  }, title), children && /*#__PURE__*/React.createElement("span", null, children)), action && /*#__PURE__*/React.createElement("div", {
    className: "az-alert__action"
  }, action));
}
Object.assign(__ds_scope, { Alert });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Alert.jsx", error: String((e && e.message) || e) }); }

// components/feedback/Badge.jsx
try { (() => {
function Badge({
  tone = 'neutral',
  dot,
  icon,
  children,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("span", {
    className: ('az-badge az-badge--' + tone + ' ' + className).trim(),
    style: style
  }, dot && /*#__PURE__*/React.createElement("span", {
    className: "az-badge__dot"
  }), icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: 11
  }), children);
}
Object.assign(__ds_scope, { Badge });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/Badge.jsx", error: String((e && e.message) || e) }); }

// components/feedback/EmptyState.jsx
try { (() => {
function EmptyState({
  icon = 'inbox',
  title,
  description,
  action,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-empty ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: 28
  }), /*#__PURE__*/React.createElement("h3", {
    className: "az-empty__title"
  }, title), description && /*#__PURE__*/React.createElement("p", {
    className: "az-empty__text"
  }, description), action && /*#__PURE__*/React.createElement("div", {
    className: "az-empty__action"
  }, action));
}
Object.assign(__ds_scope, { EmptyState });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/feedback/EmptyState.jsx", error: String((e && e.message) || e) }); }

// components/filters/DateRangePicker.jsx
try { (() => {
const DRP_MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
const drpDay = d => new Date(d.getFullYear(), d.getMonth(), d.getDate());
const drpFmt = d => d ? d.getDate() + ' ' + DRP_MONTHS[d.getMonth()] : '…';
const DRP_PRESETS = [['Last 24 hours', 1], ['Last 7 days', 7], ['Last 30 days', 30], ['Last 90 days', 90]];
function DateRangePicker({
  value,
  defaultValue,
  onChange,
  today: todayProp,
  open: forced,
  className = '',
  style
}) {
  const today = drpDay(todayProp ? new Date(todayProp) : new Date());
  const init = defaultValue || {
    from: new Date(today.getTime() - 6 * 864e5),
    to: today
  };
  const [inner, setInner] = React.useState(init);
  const range = value || inner;
  const [open, setOpen] = React.useState(false);
  const [month, setMonth] = React.useState(new Date(range.to.getFullYear(), range.to.getMonth(), 1));
  const [picking, setPicking] = React.useState(null);
  const isOpen = forced != null ? forced : open;
  const commit = r => {
    setInner(r);
    onChange && onChange(r);
  };
  const pick = d => {
    if (!picking) {
      setPicking(d);
      commit({
        from: d,
        to: null
      });
    } else {
      const r = d < picking ? {
        from: d,
        to: picking
      } : {
        from: picking,
        to: d
      };
      setPicking(null);
      commit(r);
    }
  };
  const preset = n => {
    const r = {
      from: new Date(today.getTime() - (n - 1) * 864e5),
      to: today
    };
    setPicking(null);
    commit(r);
    setMonth(new Date(today.getFullYear(), today.getMonth(), 1));
  };
  const first = (month.getDay() + 6) % 7;
  const days = new Date(month.getFullYear(), month.getMonth() + 1, 0).getDate();
  const cells = Array.from({
    length: first + days
  }, (_, i) => i < first ? null : new Date(month.getFullYear(), month.getMonth(), i - first + 1));
  const span = range.to ? Math.round((range.to - range.from) / 864e5) + 1 : null;
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-drp ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("button", {
    className: "az-drp__trigger",
    onClick: () => setOpen(o => !o)
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "calendar",
    size: 14
  }), drpFmt(range.from), " \u2013 ", drpFmt(range.to), /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-down",
    size: 14
  })), isOpen && /*#__PURE__*/React.createElement("div", {
    className: 'az-drp__pop' + (forced ? ' az-drp__pop--static' : ''),
    style: forced ? {
      position: 'absolute'
    } : undefined
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-drp__presets"
  }, DRP_PRESETS.map(([l, n]) => /*#__PURE__*/React.createElement("button", {
    key: n,
    className: 'az-drp__preset' + (span === n && range.to && +range.to === +today ? ' az-drp__preset--active' : ''),
    onClick: () => preset(n)
  }, l))), /*#__PURE__*/React.createElement("div", {
    className: "az-drp__cal"
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-drp__head"
  }, /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "chevron-left",
    label: "Previous month",
    onClick: () => setMonth(new Date(month.getFullYear(), month.getMonth() - 1, 1))
  }), /*#__PURE__*/React.createElement("span", null, DRP_MONTHS[month.getMonth()], " ", month.getFullYear()), /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "chevron-right",
    label: "Next month",
    onClick: () => setMonth(new Date(month.getFullYear(), month.getMonth() + 1, 1))
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-drp__grid"
  }, ['Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa', 'Su'].map(d => /*#__PURE__*/React.createElement("span", {
    key: d,
    className: "az-drp__dow"
  }, d)), cells.map((d, i) => {
    if (!d) return /*#__PURE__*/React.createElement("span", {
      key: i,
      className: "az-drp__day az-drp__day--out"
    });
    const edge = +d === +range.from || range.to && +d === +range.to;
    const inR = range.to && d > range.from && d < range.to;
    return /*#__PURE__*/React.createElement("button", {
      key: i,
      className: 'az-drp__day' + (edge ? ' az-drp__day--edge' : inR ? ' az-drp__day--in' : '') + (+d === +today ? ' az-drp__day--today' : ''),
      onClick: () => pick(d)
    }, d.getDate());
  })))));
}
Object.assign(__ds_scope, { DateRangePicker });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/filters/DateRangePicker.jsx", error: String((e && e.message) || e) }); }

// components/filters/TagInput.jsx
try { (() => {
function TagInput({
  label,
  hint,
  value,
  defaultValue = [],
  onChange,
  placeholder = 'Add…',
  mono,
  suggestions = [],
  className = '',
  style
}) {
  const [inner, setInner] = React.useState(defaultValue);
  const [text, setText] = React.useState('');
  const tags = value != null ? value : inner;
  const set = t => {
    setInner(t);
    onChange && onChange(t);
  };
  const add = v => {
    const s = v.trim();
    if (s && !tags.includes(s)) set([...tags, s]);
    setText('');
  };
  const listId = React.useId ? React.useId() : undefined;
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-field ' + className).trim(),
    style: style
  }, label && /*#__PURE__*/React.createElement("span", {
    className: "az-field__label"
  }, label), /*#__PURE__*/React.createElement("div", {
    className: "az-tags",
    onClick: e => {
      const i = e.currentTarget.querySelector('input');
      i && i.focus();
    }
  }, tags.map(t => /*#__PURE__*/React.createElement("span", {
    key: t,
    className: 'az-tag' + (mono ? ' az-tag--mono' : '')
  }, t, /*#__PURE__*/React.createElement("button", {
    className: "az-tag__x",
    "aria-label": 'Remove ' + t,
    onClick: () => set(tags.filter(x => x !== t))
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "x",
    size: 12
  })))), /*#__PURE__*/React.createElement("input", {
    className: "az-tags__input",
    value: text,
    placeholder: tags.length ? '' : placeholder,
    list: suggestions.length ? listId : undefined,
    onChange: e => setText(e.target.value),
    onKeyDown: e => {
      if (e.key === 'Enter' || e.key === ',') {
        e.preventDefault();
        add(text);
      } else if (e.key === 'Backspace' && !text && tags.length) set(tags.slice(0, -1));
    },
    onBlur: () => text && add(text)
  }), suggestions.length > 0 && /*#__PURE__*/React.createElement("datalist", {
    id: listId
  }, suggestions.map(s => /*#__PURE__*/React.createElement("option", {
    key: s,
    value: s
  })))), hint && /*#__PURE__*/React.createElement("span", {
    className: "az-field__hint"
  }, hint));
}
Object.assign(__ds_scope, { TagInput });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/filters/TagInput.jsx", error: String((e && e.message) || e) }); }

// components/forms/Input.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Input({
  label,
  hint,
  error,
  icon,
  mono,
  size,
  id,
  className = '',
  style,
  ...rest
}) {
  const autoId = React.useId();
  const iid = id || (label ? autoId : undefined);
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-field' + (error ? ' az-field--error' : '') + (className ? ' ' + className : ''),
    style: style
  }, label && /*#__PURE__*/React.createElement("label", {
    className: "az-field__label",
    htmlFor: iid
  }, label), /*#__PURE__*/React.createElement("span", {
    className: 'az-input-wrap' + (icon ? ' az-input-wrap--icon' : '')
  }, icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon,
    size: 15
  }), /*#__PURE__*/React.createElement("input", _extends({
    id: iid,
    className: 'az-input' + (mono ? ' az-input--mono' : '') + (size === 'sm' ? ' az-input--sm' : ''),
    "aria-invalid": error ? 'true' : undefined
  }, rest))), (error || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-field__hint"
  }, error || hint));
}
Object.assign(__ds_scope, { Input });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Input.jsx", error: String((e && e.message) || e) }); }

// components/forms/Select.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Select({
  label,
  hint,
  error,
  options = [],
  size,
  id,
  className = '',
  style,
  ...rest
}) {
  const autoId = React.useId();
  const iid = id || (label ? autoId : undefined);
  const opts = options.map(o => typeof o === 'string' ? {
    value: o,
    label: o
  } : o);
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-field' + (error ? ' az-field--error' : '') + (className ? ' ' + className : ''),
    style: style
  }, label && /*#__PURE__*/React.createElement("label", {
    className: "az-field__label",
    htmlFor: iid
  }, label), /*#__PURE__*/React.createElement("span", {
    className: "az-input-wrap az-select-wrap"
  }, /*#__PURE__*/React.createElement("select", _extends({
    id: iid,
    className: 'az-input az-select' + (size === 'sm' ? ' az-input--sm' : '')
  }, rest), opts.map(o => /*#__PURE__*/React.createElement("option", {
    key: o.value,
    value: o.value
  }, o.label))), /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-down",
    size: 15,
    className: "az-select__chev"
  })), (error || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-field__hint"
  }, error || hint));
}
Object.assign(__ds_scope, { Select });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Select.jsx", error: String((e && e.message) || e) }); }

// components/forms/Textarea.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
function Textarea({
  label,
  hint,
  error,
  mono,
  rows = 4,
  id,
  className = '',
  style,
  ...rest
}) {
  const tid = id || (label ? 'az-' + String(label).toLowerCase().replace(/[^a-z0-9]+/g, '-') : undefined);
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-field' + (error ? ' az-field--error' : '') + (className ? ' ' + className : ''),
    style: style
  }, label && /*#__PURE__*/React.createElement("label", {
    className: "az-field__label",
    htmlFor: tid
  }, label), /*#__PURE__*/React.createElement("textarea", _extends({
    id: tid,
    rows: rows,
    className: 'az-input az-textarea' + (mono ? ' az-input--mono' : ''),
    "aria-invalid": error ? 'true' : undefined
  }, rest)), (error || hint) && /*#__PURE__*/React.createElement("span", {
    className: "az-field__hint"
  }, error || hint));
}
Object.assign(__ds_scope, { Textarea });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/forms/Textarea.jsx", error: String((e && e.message) || e) }); }

// components/identity/Avatar.jsx
try { (() => {
function _extends() { return _extends = Object.assign ? Object.assign.bind() : function (n) { for (var e = 1; e < arguments.length; e++) { var t = arguments[e]; for (var r in t) ({}).hasOwnProperty.call(t, r) && (n[r] = t[r]); } return n; }, _extends.apply(null, arguments); }
const AV_STATUS = {
  online: 'var(--az-good)',
  busy: 'var(--az-warn)',
  offline: 'var(--az-faint)'
};
function Avatar({
  name = '',
  src,
  size = 32,
  tone = 'neutral',
  status,
  className = '',
  style
}) {
  const initials = name.split(/\s+/).filter(Boolean).map(p => p[0]).slice(0, 2).join('').toUpperCase();
  return /*#__PURE__*/React.createElement("span", {
    className: 'az-av' + (tone !== 'neutral' ? ' az-av--' + tone : '') + (className ? ' ' + className : ''),
    style: {
      width: size,
      height: size,
      fontSize: Math.round(size * 0.36),
      ...style
    },
    title: name,
    "aria-label": name,
    role: "img"
  }, src ? /*#__PURE__*/React.createElement("img", {
    src: src,
    alt: ""
  }) : initials, status && /*#__PURE__*/React.createElement("span", {
    className: "az-av__status",
    style: {
      background: AV_STATUS[status]
    }
  }));
}
function AvatarGroup({
  people = [],
  max = 4,
  size = 28,
  className = '',
  style
}) {
  const shown = people.slice(0, max);
  const rest = people.length - shown.length;
  return /*#__PURE__*/React.createElement("span", {
    className: ('az-av-group ' + className).trim(),
    style: style
  }, shown.map((p, i) => /*#__PURE__*/React.createElement(Avatar, _extends({
    key: i,
    size: size
  }, typeof p === 'string' ? {
    name: p
  } : p))), rest > 0 && /*#__PURE__*/React.createElement("span", {
    className: "az-av az-av--more",
    style: {
      width: size,
      height: size,
      fontSize: Math.round(size * 0.34)
    }
  }, "+", rest));
}
Object.assign(__ds_scope, { Avatar, AvatarGroup });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/identity/Avatar.jsx", error: String((e && e.message) || e) }); }

// components/loading/Skeleton.jsx
try { (() => {
function Skeleton({
  width = '100%',
  height = 12,
  circle,
  lines,
  gap = 8,
  className = '',
  style
}) {
  if (lines) {
    return /*#__PURE__*/React.createElement("span", {
      className: ('az-skel-lines ' + className).trim(),
      style: {
        gap,
        ...style
      },
      "aria-hidden": "true"
    }, Array.from({
      length: lines
    }, (_, i) => /*#__PURE__*/React.createElement("span", {
      key: i,
      className: "az-skel",
      style: {
        width: i === lines - 1 ? '60%' : '100%',
        height
      }
    })));
  }
  return /*#__PURE__*/React.createElement("span", {
    className: 'az-skel' + (circle ? ' az-skel--circle' : '') + (className ? ' ' + className : ''),
    style: {
      width,
      height: circle ? width : height,
      ...style
    },
    "aria-hidden": "true"
  });
}
function Spinner({
  size = 16,
  label,
  className = '',
  style
}) {
  const s = /*#__PURE__*/React.createElement("span", {
    className: "az-spinner",
    style: {
      width: size,
      height: size,
      borderWidth: size >= 24 ? 3 : 2
    },
    role: "status",
    "aria-label": label || 'Loading'
  });
  if (!label) return /*#__PURE__*/React.createElement("span", {
    className: className,
    style: {
      display: 'inline-flex',
      ...style
    }
  }, s);
  return /*#__PURE__*/React.createElement("span", {
    className: ('az-spinner-wrap ' + className).trim(),
    style: style
  }, s, label);
}
Object.assign(__ds_scope, { Skeleton, Spinner });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/loading/Skeleton.jsx", error: String((e && e.message) || e) }); }

// components/navigation/Sidebar.jsx
try { (() => {
function Sidebar({
  items = [],
  active,
  onSelect,
  workspace,
  workspaces = [],
  onWorkspaceChange,
  workspaceLabel = 'Tenant',
  status = 'Connected to control plane',
  brand,
  navLabel = 'Main navigation',
  user,
  onSignOut,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("aside", {
    className: ('az-sidebar ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-sidebar__brand"
  }, brand || /*#__PURE__*/React.createElement(__ds_scope.Logo, {
    size: 30
  })), workspaces.length > 0 && /*#__PURE__*/React.createElement("div", {
    className: "az-sidebar__workspace"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-eyebrow"
  }, workspaceLabel), /*#__PURE__*/React.createElement(__ds_scope.Select, {
    size: "sm",
    value: workspace,
    options: workspaces,
    onChange: e => onWorkspaceChange && onWorkspaceChange(e.target.value)
  })), /*#__PURE__*/React.createElement("nav", {
    className: "az-nav",
    "aria-label": navLabel
  }, items.map(it => /*#__PURE__*/React.createElement("button", {
    key: it.id,
    className: 'az-nav__item' + (it.id === active ? ' az-nav__item--active' : ''),
    "aria-current": it.id === active ? 'page' : undefined,
    onClick: () => onSelect && onSelect(it.id)
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: it.icon,
    size: 17
  }), /*#__PURE__*/React.createElement("span", null, it.label), it.badge != null && /*#__PURE__*/React.createElement("span", {
    className: "az-nav__badge"
  }, it.badge)))), /*#__PURE__*/React.createElement("div", {
    className: "az-sidebar__foot"
  }, status && /*#__PURE__*/React.createElement("div", {
    className: "az-connected"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-dot az-dot--pulse"
  }), status), user && /*#__PURE__*/React.createElement("div", {
    className: "az-profile"
  }, /*#__PURE__*/React.createElement(__ds_scope.Avatar, {
    name: user.name,
    size: 32
  }), /*#__PURE__*/React.createElement("span", {
    className: "az-profile__meta"
  }, /*#__PURE__*/React.createElement("b", null, user.name), /*#__PURE__*/React.createElement("small", null, user.role)), /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "log-out",
    label: "Sign out",
    size: 15,
    onClick: onSignOut
  }))));
}
Object.assign(__ds_scope, { Sidebar });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/navigation/Sidebar.jsx", error: String((e && e.message) || e) }); }

// components/navigation/Tabs.jsx
try { (() => {
function Tabs({
  items = [],
  value,
  defaultValue,
  onChange,
  className = '',
  style
}) {
  const [inner, setInner] = React.useState(defaultValue != null ? defaultValue : items[0] && items[0].id);
  const active = value != null ? value : inner;
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-tabs ' + className).trim(),
    role: "tablist",
    style: style
  }, items.map(it => /*#__PURE__*/React.createElement("button", {
    key: it.id,
    role: "tab",
    "aria-selected": it.id === active,
    className: 'az-tab' + (it.id === active ? ' az-tab--active' : ''),
    onClick: () => {
      setInner(it.id);
      onChange && onChange(it.id);
    }
  }, it.label, it.count != null && /*#__PURE__*/React.createElement("span", {
    className: "az-tab__count"
  }, it.count))));
}
Object.assign(__ds_scope, { Tabs });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/navigation/Tabs.jsx", error: String((e && e.message) || e) }); }

// components/navigation/Topbar.jsx
try { (() => {
function Topbar({
  crumbs = [],
  live = true,
  children,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("header", {
    className: ('az-topbar ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("nav", {
    className: "az-crumbs",
    "aria-label": "Breadcrumb"
  }, crumbs.map((c, i) => /*#__PURE__*/React.createElement(React.Fragment, {
    key: i
  }, i > 0 && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-right",
    size: 12
  }), i === crumbs.length - 1 ? /*#__PURE__*/React.createElement("b", null, c) : /*#__PURE__*/React.createElement("span", null, c)))), /*#__PURE__*/React.createElement("div", {
    className: "az-topbar__right"
  }, live && /*#__PURE__*/React.createElement("span", {
    className: "az-live"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-dot az-dot--pulse"
  }), "LIVE"), children));
}
Object.assign(__ds_scope, { Topbar });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/navigation/Topbar.jsx", error: String((e && e.message) || e) }); }

// components/notify/Toast.jsx
try { (() => {
const TOAST_ICONS = {
  good: 'circle-check',
  warn: 'triangle-alert',
  bad: 'circle-x',
  info: 'info',
  neutral: 'bell'
};
function Toast({
  tone = 'neutral',
  title,
  children,
  icon,
  action,
  onClose,
  static: isStatic,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-toast az-toast--' + tone + (isStatic ? ' az-toast--static' : '') + (className ? ' ' + className : ''),
    role: tone === 'bad' ? 'alert' : 'status',
    style: style
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: icon || TOAST_ICONS[tone],
    size: 16
  }), /*#__PURE__*/React.createElement("div", {
    className: "az-toast__body"
  }, title && /*#__PURE__*/React.createElement("div", {
    className: "az-toast__title"
  }, title), children && /*#__PURE__*/React.createElement("div", null, children), action && /*#__PURE__*/React.createElement("div", {
    className: "az-toast__action"
  }, action)), onClose && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "x",
    label: "Dismiss",
    onClick: onClose
  }));
}
function ToastStack({
  children
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: "az-toast-stack"
  }, children);
}
Object.assign(__ds_scope, { Toast, ToastStack });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/notify/Toast.jsx", error: String((e && e.message) || e) }); }

// components/notify/Tooltip.jsx
try { (() => {
function Tooltip({
  content,
  side = 'top',
  kbd,
  open,
  children,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("span", {
    className: 'az-tip' + (side === 'bottom' ? ' az-tip--bottom' : '') + (open ? ' az-tip--open' : '') + (className ? ' ' + className : ''),
    style: style
  }, children, /*#__PURE__*/React.createElement("span", {
    className: "az-tip__bubble",
    role: "tooltip"
  }, content, kbd && /*#__PURE__*/React.createElement("span", {
    className: "az-tip__kbd"
  }, kbd)));
}
Object.assign(__ds_scope, { Tooltip });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/notify/Tooltip.jsx", error: String((e && e.message) || e) }); }

// components/overlay/Modal.jsx
try { (() => {
function Modal({
  open = true,
  title,
  description,
  onClose,
  children,
  actions,
  inline,
  width,
  className = ''
}) {
  const dialogRef = useDialogFocus(open, inline);
  React.useEffect(() => {
    if (!open || inline || !onClose) return;
    const onKey = e => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [open, inline, onClose]);
  if (!open) return null;
  const box = /*#__PURE__*/React.createElement("div", {
    className: ('az-modal' + (inline ? ' az-modal--inline ' : ' ') + className).trim(),
    role: "dialog",
    ref: dialogRef,
    tabIndex: -1,
    "aria-modal": inline ? undefined : 'true',
    "aria-label": title,
    style: {
      width
    },
    onClick: e => e.stopPropagation()
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-modal__head"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h2", {
    className: "az-modal__title"
  }, title), description && /*#__PURE__*/React.createElement("p", {
    className: "az-modal__desc"
  }, description)), onClose && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "x",
    label: "Close",
    onClick: onClose
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-modal__body"
  }, children), actions && /*#__PURE__*/React.createElement("div", {
    className: "az-modal__actions"
  }, actions));
  if (inline) return box;
  return /*#__PURE__*/React.createElement("div", {
    className: "az-backdrop",
    onClick: onClose
  }, box);
}
Object.assign(__ds_scope, { Modal });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/overlay/Modal.jsx", error: String((e && e.message) || e) }); }

// components/panels/CommandPalette.jsx
try { (() => {
function CommandPalette({
  open = true,
  items = [],
  onSelect,
  onClose,
  placeholder = 'Search or jump to…',
  inline,
  defaultQuery = '',
  className = ''
}) {
  const [q, setQ] = React.useState(defaultQuery);
  const [idx, setIdx] = React.useState(0);
  const list = items.filter(it => (it.label + ' ' + (it.hint || '') + ' ' + (it.keywords || '')).toLowerCase().includes(q.toLowerCase()));
  React.useEffect(() => {
    setIdx(0);
  }, [q]);
  if (!open) return null;
  const choose = it => {
    onSelect && onSelect(it);
    onClose && onClose();
  };
  const onKey = e => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setIdx(i => Math.min(list.length - 1, i + 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setIdx(i => Math.max(0, i - 1));
    } else if (e.key === 'Enter' && list[idx]) choose(list[idx]);else if (e.key === 'Escape' && onClose) onClose();
  };
  let last;
  const box = /*#__PURE__*/React.createElement("div", {
    className: 'az-cmdk' + (inline ? ' az-cmdk--inline' : '') + (className ? ' ' + className : ''),
    role: "dialog",
    "aria-label": "Command palette",
    onClick: e => e.stopPropagation()
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-cmdk__search"
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "search",
    size: 17
  }), /*#__PURE__*/React.createElement("input", {
    className: "az-cmdk__input",
    autoFocus: !inline,
    value: q,
    placeholder: placeholder,
    onChange: e => setQ(e.target.value),
    onKeyDown: onKey
  }), /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "esc")), /*#__PURE__*/React.createElement("div", {
    className: "az-cmdk__list",
    role: "listbox"
  }, list.length === 0 && /*#__PURE__*/React.createElement("div", {
    className: "az-cmdk__empty"
  }, "No results for \u201C", q, "\u201D"), list.map((it, i) => {
    const head = it.group && it.group !== last ? /*#__PURE__*/React.createElement("div", {
      className: "az-cmdk__group"
    }, it.group) : null;
    last = it.group;
    return /*#__PURE__*/React.createElement(React.Fragment, {
      key: it.id || it.label
    }, head, /*#__PURE__*/React.createElement("button", {
      role: "option",
      "aria-selected": i === idx,
      className: 'az-cmdk__item' + (i === idx ? ' az-cmdk__item--active' : ''),
      onMouseEnter: () => setIdx(i),
      onClick: () => choose(it)
    }, it.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: it.icon,
      size: 16
    }), /*#__PURE__*/React.createElement("span", null, it.label), it.hint && /*#__PURE__*/React.createElement("span", {
      className: "az-cmdk__hint"
    }, it.hint)));
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-cmdk__foot"
  }, /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u2191"), /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u2193"), " navigate"), /*#__PURE__*/React.createElement("span", null, /*#__PURE__*/React.createElement(__ds_scope.Kbd, null, "\u21B5"), " open")));
  if (inline) return box;
  return /*#__PURE__*/React.createElement("div", {
    className: "az-cmdk-backdrop",
    onClick: onClose
  }, box);
}
Object.assign(__ds_scope, { CommandPalette });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/panels/CommandPalette.jsx", error: String((e && e.message) || e) }); }

// components/panels/Drawer.jsx
try { (() => {
function Drawer({
  open = true,
  title,
  description,
  onClose,
  children,
  footer,
  width,
  inline,
  className = '',
  style
}) {
  const dialogRef = useDialogFocus(open, inline);
  React.useEffect(() => {
    if (!open || inline || !onClose) return;
    const k = e => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', k);
    return () => window.removeEventListener('keydown', k);
  }, [open, inline, onClose]);
  if (!open) return null;
  const panel = /*#__PURE__*/React.createElement("aside", {
    className: 'az-drawer' + (inline ? ' az-drawer--inline' : '') + (className ? ' ' + className : ''),
    role: "dialog",
    ref: dialogRef,
    tabIndex: -1,
    "aria-modal": inline ? undefined : 'true',
    "aria-label": title,
    style: {
      width,
      ...style
    },
    onClick: e => e.stopPropagation()
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-drawer__head"
  }, /*#__PURE__*/React.createElement("div", null, /*#__PURE__*/React.createElement("h2", {
    className: "az-drawer__title"
  }, title), description && /*#__PURE__*/React.createElement("p", {
    className: "az-drawer__desc"
  }, description)), onClose && /*#__PURE__*/React.createElement(__ds_scope.IconButton, {
    icon: "x",
    label: "Close",
    onClick: onClose
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-drawer__body"
  }, children), footer && /*#__PURE__*/React.createElement("div", {
    className: "az-drawer__foot"
  }, footer));
  if (inline) return panel;
  return /*#__PURE__*/React.createElement("div", {
    className: "az-drawer-backdrop",
    onClick: onClose
  }, panel);
}
function PropertyList({
  items = [],
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("dl", {
    className: ('az-props ' + className).trim(),
    style: style
  }, items.map((it, i) => /*#__PURE__*/React.createElement(React.Fragment, {
    key: i
  }, /*#__PURE__*/React.createElement("dt", null, it.label), /*#__PURE__*/React.createElement("dd", {
    className: it.mono ? 'az-props__mono' : undefined
  }, it.value))));
}
Object.assign(__ds_scope, { Drawer, PropertyList });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/panels/Drawer.jsx", error: String((e && e.message) || e) }); }

// components/selection/Pagination.jsx
try { (() => {
function pageList(page, count) {
  if (count <= 7) return Array.from({
    length: count
  }, (_, i) => i + 1);
  const set = [1, page - 1, page, page + 1, count].filter(p => p >= 1 && p <= count);
  const uniq = [...new Set(set)].sort((a, b) => a - b);
  const out = [];
  uniq.forEach((p, i) => {
    if (i && p - uniq[i - 1] > 1) out.push('gap' + p);
    out.push(p);
  });
  return out;
}
function Pagination({
  page = 1,
  pageCount = 1,
  onChange,
  total,
  pageSize,
  className = '',
  style
}) {
  const go = p => onChange && onChange(Math.min(pageCount, Math.max(1, p)));
  const from = total != null && pageSize ? (page - 1) * pageSize + 1 : null;
  const to = from != null ? Math.min(total, page * pageSize) : null;
  return /*#__PURE__*/React.createElement("nav", {
    className: ('az-pager ' + className).trim(),
    "aria-label": "Pagination",
    style: style
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-pager__range"
  }, from != null ? from + '–' + to + ' of ' + total : 'Page ' + page + ' of ' + pageCount), /*#__PURE__*/React.createElement("div", {
    className: "az-pager__pages"
  }, /*#__PURE__*/React.createElement("button", {
    className: "az-pager__btn",
    disabled: page <= 1,
    onClick: () => go(page - 1),
    "aria-label": "Previous page"
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-left",
    size: 15
  })), pageList(page, pageCount).map(p => typeof p === 'string' ? /*#__PURE__*/React.createElement("span", {
    key: p,
    className: "az-pager__gap"
  }, "\u2026") : /*#__PURE__*/React.createElement("button", {
    key: p,
    className: 'az-pager__btn' + (p === page ? ' az-pager__btn--active' : ''),
    "aria-current": p === page ? 'page' : undefined,
    onClick: () => go(p)
  }, p)), /*#__PURE__*/React.createElement("button", {
    className: "az-pager__btn",
    disabled: page >= pageCount,
    onClick: () => go(page + 1),
    "aria-label": "Next page"
  }, /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: "chevron-right",
    size: 15
  }))));
}
Object.assign(__ds_scope, { Pagination });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/selection/Pagination.jsx", error: String((e && e.message) || e) }); }

// components/selection/SegmentedControl.jsx
try { (() => {
function SegmentedControl({
  options = [],
  value,
  defaultValue,
  onChange,
  mono,
  block,
  className = '',
  style,
  'aria-label': ariaLabel
}) {
  const opts = options.map(o => typeof o === 'string' ? {
    value: o,
    label: o
  } : o);
  const [inner, setInner] = React.useState(defaultValue != null ? defaultValue : opts[0] && opts[0].value);
  const cur = value != null ? value : inner;
  return /*#__PURE__*/React.createElement("div", {
    className: 'az-seg' + (mono ? ' az-seg--mono' : '') + (block ? ' az-seg--block' : '') + (className ? ' ' + className : ''),
    role: "radiogroup",
    "aria-label": ariaLabel,
    style: style
  }, opts.map(o => /*#__PURE__*/React.createElement("button", {
    key: o.value,
    role: "radio",
    "aria-checked": o.value === cur,
    className: 'az-seg__item' + (o.value === cur ? ' az-seg__item--active' : ''),
    onClick: () => {
      setInner(o.value);
      onChange && onChange(o.value);
    }
  }, o.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
    name: o.icon,
    size: 14
  }), o.label)));
}
Object.assign(__ds_scope, { SegmentedControl });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/selection/SegmentedControl.jsx", error: String((e && e.message) || e) }); }

// components/selection/Stepper.jsx
try { (() => {
function Stepper({
  steps = [],
  current = 0,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("ol", {
    className: ('az-stepper ' + className).trim(),
    style: style
  }, steps.map((s, i) => {
    const st = typeof s === 'string' ? {
      label: s
    } : s;
    const state = i < current ? 'done' : i === current ? 'current' : 'todo';
    return /*#__PURE__*/React.createElement("li", {
      key: i,
      className: 'az-step az-step--' + state,
      "aria-current": state === 'current' ? 'step' : undefined
    }, /*#__PURE__*/React.createElement("span", {
      className: "az-step__bar"
    }), /*#__PURE__*/React.createElement("span", {
      className: "az-step__head"
    }, /*#__PURE__*/React.createElement("span", {
      className: "az-step__num"
    }, state === 'done' ? /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: "check",
      size: 11
    }) : i + 1), /*#__PURE__*/React.createElement("span", {
      className: "az-step__label"
    }, st.label)), st.description && /*#__PURE__*/React.createElement("span", {
      className: "az-step__desc"
    }, st.description));
  }));
}
Object.assign(__ds_scope, { Stepper });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/selection/Stepper.jsx", error: String((e && e.message) || e) }); }

// components/settings/SettingsLayout.jsx
try { (() => {
function SettingsLayout({
  items = [],
  value,
  defaultValue,
  onChange,
  variant = 'side',
  children,
  className = '',
  style
}) {
  const [inner, setInner] = React.useState(defaultValue != null ? defaultValue : items[0] && items[0].id);
  const active = value != null ? value : inner;
  const select = id => {
    setInner(id);
    onChange && onChange(id);
  };
  const body = typeof children === 'function' ? children(active) : children;
  if (variant === 'tabs') {
    return /*#__PURE__*/React.createElement("div", {
      className: ('az-settings az-settings--tabs ' + className).trim(),
      style: style
    }, /*#__PURE__*/React.createElement(__ds_scope.Tabs, {
      items: items,
      value: active,
      onChange: select
    }), /*#__PURE__*/React.createElement("div", {
      className: "az-settings__body"
    }, body));
  }
  let lastGroup;
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-settings ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("nav", {
    className: "az-settings__nav",
    "aria-label": "Settings"
  }, items.map(it => {
    const head = it.group && it.group !== lastGroup ? /*#__PURE__*/React.createElement("div", {
      className: "az-settings__group"
    }, it.group) : null;
    lastGroup = it.group;
    return /*#__PURE__*/React.createElement(React.Fragment, {
      key: it.id
    }, head, /*#__PURE__*/React.createElement("button", {
      className: 'az-settings__item' + (it.id === active ? ' az-settings__item--active' : ''),
      "aria-current": it.id === active ? 'page' : undefined,
      onClick: () => select(it.id)
    }, it.icon && /*#__PURE__*/React.createElement(__ds_scope.Icon, {
      name: it.icon,
      size: 15
    }), it.label));
  })), /*#__PURE__*/React.createElement("div", {
    className: "az-settings__body"
  }, body));
}
Object.assign(__ds_scope, { SettingsLayout });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/settings/SettingsLayout.jsx", error: String((e && e.message) || e) }); }

// components/settings/SettingsSection.jsx
try { (() => {
function SettingsSection({
  title,
  description,
  children,
  footer,
  footerNote,
  form,
  danger,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("section", {
    className: 'az-card az-set-section' + (danger ? ' az-set-section--danger' : '') + (className ? ' ' + className : ''),
    style: style
  }, (title || description) && /*#__PURE__*/React.createElement("div", {
    className: "az-set-section__head"
  }, title && /*#__PURE__*/React.createElement("h3", {
    className: "az-set-section__title"
  }, title), description && /*#__PURE__*/React.createElement("p", {
    className: "az-set-section__desc"
  }, description)), /*#__PURE__*/React.createElement("div", {
    className: 'az-set-section__body' + (form ? ' az-set-section__body--form' : '')
  }, children), footer && /*#__PURE__*/React.createElement("div", {
    className: "az-set-section__foot"
  }, footerNote && /*#__PURE__*/React.createElement("span", {
    className: "az-set-section__foot-note"
  }, footerNote), footer));
}
function SettingsRow({
  label,
  description,
  children,
  className = '',
  style
}) {
  return /*#__PURE__*/React.createElement("div", {
    className: ('az-set-row ' + className).trim(),
    style: style
  }, /*#__PURE__*/React.createElement("div", {
    className: "az-set-row__text"
  }, /*#__PURE__*/React.createElement("span", {
    className: "az-set-row__label"
  }, label), description && /*#__PURE__*/React.createElement("span", {
    className: "az-set-row__desc"
  }, description)), children && /*#__PURE__*/React.createElement("div", {
    className: "az-set-row__control"
  }, children));
}
Object.assign(__ds_scope, { SettingsSection, SettingsRow });
})(); } catch (e) { __ds_ns.__errors.push({ path: "components/settings/SettingsSection.jsx", error: String((e && e.message) || e) }); }

export const { Icon, Button, IconButton, Logo, ProductLogo, Checkbox, Radio, RadioGroup, Switch, ChartLegend, CodeBlock, LineChart, Meter, Sparkline, StatCard, Table, DropdownMenu, Kbd, Alert, Badge, EmptyState, Input, Select, Textarea, Avatar, Skeleton, Spinner, Sidebar, Tabs, Topbar, Toast, ToastStack, Tooltip, Modal, CommandPalette, Drawer, PropertyList, Pagination, SegmentedControl, Stepper, SettingsLayout, SettingsSection, SettingsRow } = __ds_scope;
