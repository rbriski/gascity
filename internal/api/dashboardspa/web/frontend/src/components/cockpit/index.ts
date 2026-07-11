// Cockpit instruments: the DOM/SVG gauges, meters, and readouts that make up
// the dashboard home. Every instrument deep-links and carries status by shape
// and position, never by the maroon accent (reserved for the needs-you strip).

export { Gauge, type GaugeProps } from './Gauge';
export { Odometer, odometerColumns, type OdometerProps } from './Odometer';
export {
  PipelineBar,
  pipelineWidths,
  type PipelineBarProps,
  type PipelineSegment,
} from './PipelineBar';
export { VUBank, fillPct, type VUBankProps, type VUMeter } from './VUBank';
export {
  RunRings,
  ringDashoffset,
  RING_R,
  RING_CIRC,
  type RunRingsProps,
  type RunRing,
} from './RunRings';
export { StatusLamps, type StatusLampsProps, type StatusLamp, type LampTone } from './StatusLamps';
export { Oscilloscope, type OscilloscopeProps } from './Oscilloscope';

export * from './arc';
export * from './format';
export * from './scopeBuffer';
export * from './useInkPalette';
