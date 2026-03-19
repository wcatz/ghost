// AudioWorklet processor that converts Float32 audio samples to Int16 PCM
// for AssemblyAI real-time streaming (requires 16-bit PCM at 16kHz).
// Buffers 4096 samples (~256ms at 16kHz) before sending to match what
// AssemblyAI expects — the default 128-sample render quantum is too small.
class PCMProcessor extends AudioWorkletProcessor {
	constructor() {
		super();
		this._buffer = new Float32Array(4096);
		this._pos = 0;
	}

	process(inputs) {
		const input = inputs[0]?.[0];
		if (!input || input.length === 0) return true;

		let offset = 0;
		while (offset < input.length) {
			const remaining = this._buffer.length - this._pos;
			const toCopy = Math.min(remaining, input.length - offset);
			this._buffer.set(input.subarray(offset, offset + toCopy), this._pos);
			this._pos += toCopy;
			offset += toCopy;

			if (this._pos >= this._buffer.length) {
				const pcm16 = new Int16Array(this._buffer.length);
				for (let i = 0; i < this._buffer.length; i++) {
					const s = Math.max(-1, Math.min(1, this._buffer[i]));
					pcm16[i] = s < 0 ? s * 0x8000 : s * 0x7FFF;
				}
				this.port.postMessage(pcm16.buffer, [pcm16.buffer]);
				this._pos = 0;
			}
		}
		return true;
	}
}

registerProcessor('pcm-processor', PCMProcessor);
