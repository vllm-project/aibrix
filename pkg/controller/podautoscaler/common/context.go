package common

// ScalingContext defines the generalized common that holds all necessary data for scaling calculations.
type ScalingContext interface {
	GetTargetValue() float64
	GetUpFluctuationTolerance() float64
	GetDownFluctuationTolerance() float64
	GetMaxScaleUpRate() float64
	GetMaxScaleDownRate() float64
	GetCurrentUsePerPod() float64
}

// BaseScalingContext provides a base implementation of the ScalingContext interface.
type BaseScalingContext struct {
	currentUsePerPod float64
	targetValue      float64
	upTolerance      float64
	downTolerance    float64
}

func (b BaseScalingContext) SetCurrentUsePerPod(value float64) {
	b.currentUsePerPod = value
}

func (b *BaseScalingContext) GetUpFluctuationTolerance() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetDownFluctuationTolerance() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetMaxScaleUpRate() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetMaxScaleDownRate() float64 {
	//TODO implement me
	panic("implement me")
}

func (b *BaseScalingContext) GetCurrentUsePerPod() float64 {
	return b.currentUsePerPod
}

func (b *BaseScalingContext) GetTargetValue() float64 {
	return b.targetValue
}

func (b *BaseScalingContext) GetScalingTolerance() (up float64, down float64) {
	return b.upTolerance, b.downTolerance
}
