export function summarize(values){
 if(!values.length||!values.every(Number.isFinite))throw new Error('invalid_samples');
 const sorted=[...values].sort((a,b)=>a-b),middle=Math.floor(sorted.length/2);
 return {samples:values,median:sorted.length%2?sorted[middle]:(sorted[middle-1]+sorted[middle])/2,max:sorted.at(-1)};
}
